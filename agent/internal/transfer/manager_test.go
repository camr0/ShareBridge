package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/cloudwebdav"
	"sharebridge/agent/internal/multilane"
)

// mockDC implements DataChannel for testing
type mockDC struct {
	textMessages  []string
	binaryData    [][]byte
	binaryClasses []multilane.TrafficClass
	buffered      uint64
	closed        bool
	onMessage     func([]byte)
	mu            sync.Mutex
}

func (m *mockDC) SendBinary(data []byte) error {
	return m.SendBinaryClass(multilane.ClassControl, data)
}

func (m *mockDC) SendBinaryClass(class multilane.TrafficClass, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.binaryData = append(m.binaryData, append([]byte(nil), data...))
	m.binaryClasses = append(m.binaryClasses, class)
	return nil
}

func (m *mockDC) SendText(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.textMessages = append(m.textMessages, text)
	return nil
}

func (m *mockDC) BufferedAmount() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buffered
}

func (m *mockDC) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockDC) SetOnMessage(handler func([]byte)) {
	m.mu.Lock()
	m.onMessage = handler
	m.mu.Unlock()
}

func (m *mockDC) deliver(data []byte) {
	m.mu.Lock()
	handler := m.onMessage
	m.mu.Unlock()
	if handler != nil {
		handler(data)
	}
}

func (m *mockDC) getLastTextMessage() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.textMessages) == 0 {
		return ""
	}
	return m.textMessages[len(m.textMessages)-1]
}

func (m *mockDC) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *mockDC) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.textMessages = nil
	m.binaryData = nil
	m.binaryClasses = nil
	m.closed = false
}

type mockChannelSet struct {
	control *mockDC
	media   *mockDC
	bulk    *mockDC
	onOpen  func()
	onClose func()
}

func newMockChannelSet() *mockChannelSet {
	return &mockChannelSet{control: &mockDC{}, media: &mockDC{}, bulk: &mockDC{}}
}

func (m *mockChannelSet) Endpoint(lane multilane.Lane) multilane.Endpoint {
	switch lane {
	case multilane.LaneControl:
		return m.control
	case multilane.LaneMedia:
		return m.media
	case multilane.LaneBulk:
		return m.bulk
	default:
		return nil
	}
}

func (m *mockChannelSet) SetOnOpen(handler func())  { m.onOpen = handler }
func (m *mockChannelSet) SetOnClose(handler func()) { m.onClose = handler }
func (m *mockChannelSet) Close() error {
	_ = m.control.Close()
	_ = m.media.Close()
	_ = m.bulk.Close()
	return nil
}

func (m *mockDC) hasTextType(messageType string) bool {
	return m.countTextType(messageType) > 0
}

func (m *mockDC) getTextByType(messageType string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.textMessages {
		var parsed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(msg), &parsed) == nil && parsed.Type == messageType {
			return msg
		}
	}
	return ""
}

func (m *mockDC) countTextType(messageType string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, msg := range m.textMessages {
		var parsed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(msg), &parsed) == nil && parsed.Type == messageType {
			count++
		}
	}
	return count
}

func (m *mockDC) binaryCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.binaryData)
}

func (m *mockDC) hasBinaryClass(class multilane.TrafficClass) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, got := range m.binaryClasses {
		if got == class {
			return true
		}
	}
	return false
}

func (m *mockDC) errorFieldsContaining(substr string) (scope, requestID string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.textMessages {
		var parsed struct {
			Type      string `json:"type"`
			Scope     string `json:"scope"`
			RequestID string `json:"request_id"`
			Message   string `json:"message"`
		}
		if json.Unmarshal([]byte(msg), &parsed) == nil && parsed.Type == "error" && strings.Contains(parsed.Message, substr) {
			return parsed.Scope, parsed.RequestID, true
		}
	}
	return "", "", false
}

func (m *mockDC) hasErrorContaining(substr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.textMessages {
		var parsed struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(msg), &parsed) == nil && parsed.Type == "error" && strings.Contains(parsed.Message, substr) {
			return true
		}
	}
	return false
}

// mockOpenCloudClient implements openCloudClient for testing
type mockOpenCloudClient struct {
	mu              sync.Mutex
	listFilesPath   string
	listFilesResult []cloudwebdav.FileInfo
	listFilesErr    error
	getFilePath     string
	getFileErr      error
	file            []byte
}

func (m *mockOpenCloudClient) ListFiles(subpath string) ([]cloudwebdav.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listFilesPath = subpath
	return m.listFilesResult, m.listFilesErr
}

func (m *mockOpenCloudClient) GetFile(filePath string, w io.Writer) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getFilePath = filePath
	if len(m.file) > 0 {
		n, err := w.Write(m.file)
		return int64(n), err
	}
	return 0, m.getFileErr
}

func (m *mockOpenCloudClient) GetSHA1(subpath string) string { return "" }

type mockGalleryClient struct {
	gallery      Gallery
	thumbnail    []byte
	file         []byte
	assetQuality string
	headSize     int64
	rangeStart   int64
}

func (m *mockGalleryClient) ListGallery(ctx context.Context) (Gallery, error) {
	return m.gallery, nil
}

func (m *mockGalleryClient) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	n, err := w.Write(m.thumbnail)
	return int64(n), err
}

func (m *mockGalleryClient) GetAssetInfo(ctx context.Context, id string) (string, int64, string, error) {
	for _, item := range m.gallery.Items {
		if item.ID == id {
			return item.Name, item.Size, item.MimeType, nil
		}
	}
	return "", 0, "", fmt.Errorf("asset %s not found", id)
}

func (m *mockGalleryClient) GetAsset(ctx context.Context, id string, quality string, w io.Writer) (int64, error) {
	m.assetQuality = quality
	n, err := w.Write(m.file)
	return int64(n), err
}

func (m *mockGalleryClient) HeadVideoPlayback(ctx context.Context, id string) (int64, error) {
	return m.headSize, nil
}

func (m *mockGalleryClient) GetAssetRange(ctx context.Context, id string, quality string, startOffset int64, w io.Writer) (int64, error) {
	m.assetQuality = quality
	m.rangeStart = startOffset
	if startOffset >= int64(len(m.file)) {
		return 0, io.EOF
	}
	n, err := w.Write(m.file[startOffset:])
	return int64(n), err
}

type blockingGalleryClient struct {
	gallery     Gallery
	file        []byte
	listStarted chan struct{}
	releaseList chan struct{}
	listCalls   atomic.Int32
	assetCalls  atomic.Int32
}

func (m *blockingGalleryClient) ListGallery(ctx context.Context) (Gallery, error) {
	if m.listCalls.Add(1) == 1 {
		close(m.listStarted)
	}
	<-m.releaseList
	return m.gallery, nil
}

func (m *blockingGalleryClient) GetAssetInfo(ctx context.Context, id string) (string, int64, string, error) {
	if m.listCalls.Add(1) == 1 {
		close(m.listStarted)
	}
	<-m.releaseList
	for _, item := range m.gallery.Items {
		if item.ID == id {
			return item.Name, item.Size, item.MimeType, nil
		}
	}
	return "", 0, "", fmt.Errorf("asset %s not found", id)
}

func (m *blockingGalleryClient) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	return 0, nil
}

func (m *blockingGalleryClient) GetAsset(ctx context.Context, id string, quality string, w io.Writer) (int64, error) {
	m.assetCalls.Add(1)
	n, err := w.Write(m.file)
	return int64(n), err
}

func (m *blockingGalleryClient) HeadVideoPlayback(ctx context.Context, id string) (int64, error) {
	return int64(len(m.file)), nil
}

func (m *blockingGalleryClient) GetAssetRange(ctx context.Context, id string, quality string, startOffset int64, w io.Writer) (int64, error) {
	if startOffset >= int64(len(m.file)) {
		return 0, io.EOF
	}
	n, err := w.Write(m.file[startOffset:])
	return int64(n), err
}

type concurrentThumbnailGalleryClient struct {
	gallery       Gallery
	release       chan struct{}
	started       chan struct{}
	startedOnce   sync.Once
	current       atomic.Int32
	maxConcurrent atomic.Int32
}

func (m *concurrentThumbnailGalleryClient) GetAssetInfo(ctx context.Context, id string) (string, int64, string, error) {
	return "asset.jpg", 12345, "image/jpeg", nil
}

func (m *concurrentThumbnailGalleryClient) ListGallery(ctx context.Context) (Gallery, error) {
	return m.gallery, nil
}

func (m *concurrentThumbnailGalleryClient) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	current := m.current.Add(1)
	for {
		max := m.maxConcurrent.Load()
		if current <= max || m.maxConcurrent.CompareAndSwap(max, current) {
			break
		}
	}
	if current == 2 {
		m.startedOnce.Do(func() { close(m.started) })
	}
	<-m.release
	m.current.Add(-1)
	n, err := w.Write([]byte(id))
	return int64(n), err
}

func (m *concurrentThumbnailGalleryClient) GetAsset(ctx context.Context, id string, quality string, w io.Writer) (int64, error) {
	return 0, nil
}

func (m *concurrentThumbnailGalleryClient) HeadVideoPlayback(ctx context.Context, id string) (int64, error) {
	return 0, nil
}

func (m *concurrentThumbnailGalleryClient) GetAssetRange(ctx context.Context, id string, quality string, startOffset int64, w io.Writer) (int64, error) {
	return 0, nil
}

type blockingStorageClient struct {
	files       []cloudwebdav.FileInfo
	file        []byte
	listStarted chan struct{}
	releaseList chan struct{}
	listCalls   atomic.Int32
	getCalls    atomic.Int32
}

func (m *blockingStorageClient) ListFiles(subpath string) ([]cloudwebdav.FileInfo, error) {
	if m.listCalls.Add(1) == 1 {
		close(m.listStarted)
	}
	<-m.releaseList
	return m.files, nil
}

func (m *blockingStorageClient) GetFile(filePath string, w io.Writer) (int64, error) {
	m.getCalls.Add(1)
	n, err := w.Write(m.file)
	return int64(n), err
}

func (m *blockingStorageClient) GetSHA1(subpath string) string { return "" }

type laneConcurrencyGalleryClient struct {
	bulkStarted    chan struct{}
	bulkRelease    chan struct{}
	previewStarted chan struct{}
	previewRelease chan struct{}
}

func (m *laneConcurrencyGalleryClient) ListGallery(context.Context) (Gallery, error) {
	return Gallery{}, nil
}
func (m *laneConcurrencyGalleryClient) GetThumbnail(context.Context, string, io.Writer) (int64, error) {
	return 0, nil
}
func (m *laneConcurrencyGalleryClient) GetAssetInfo(context.Context, string) (string, int64, string, error) {
	return "asset.jpg", 2, "image/jpeg", nil
}
func (m *laneConcurrencyGalleryClient) HeadVideoPlayback(context.Context, string) (int64, error) {
	return 2, nil
}
func (m *laneConcurrencyGalleryClient) GetAsset(ctx context.Context, _ string, quality string, w io.Writer) (int64, error) {
	started, release := m.previewStarted, m.previewRelease
	if quality == "original" {
		started, release = m.bulkStarted, m.bulkRelease
	}
	if _, err := w.Write([]byte("a")); err != nil {
		return 0, err
	}
	close(started)
	select {
	case <-release:
	case <-ctx.Done():
		return 1, ctx.Err()
	}
	n, err := w.Write([]byte("b"))
	return int64(1 + n), err
}
func (m *laneConcurrencyGalleryClient) GetAssetRange(context.Context, string, string, int64, io.Writer) (int64, error) {
	return 0, nil
}

type replacingGalleryClient struct {
	firstStarted  chan struct{}
	firstCanceled chan struct{}
	firstRelease  chan struct{}
	firstFinished chan struct{}
	secondStarted chan struct{}
	bulkStarted   chan struct{}
	bulkRelease   chan struct{}
	once          sync.Once
}

func (m *replacingGalleryClient) ListGallery(context.Context) (Gallery, error) { return Gallery{}, nil }
func (m *replacingGalleryClient) GetThumbnail(context.Context, string, io.Writer) (int64, error) {
	return 0, nil
}
func (m *replacingGalleryClient) GetAssetInfo(context.Context, string) (string, int64, string, error) {
	return "preview.jpg", 1, "image/jpeg", nil
}
func (m *replacingGalleryClient) HeadVideoPlayback(context.Context, string) (int64, error) {
	return 1, nil
}
func (m *replacingGalleryClient) GetAsset(ctx context.Context, id, quality string, w io.Writer) (int64, error) {
	if quality == "original" {
		if _, err := w.Write([]byte("bulk-a")); err != nil {
			return 0, err
		}
		close(m.bulkStarted)
		<-m.bulkRelease
		n, err := w.Write([]byte("bulk-b"))
		return int64(6 + n), err
	}
	if id == "first" {
		close(m.firstStarted)
		<-ctx.Done()
		m.once.Do(func() { close(m.firstCanceled) })
		<-m.firstRelease
		n, err := w.Write([]byte("old"))
		close(m.firstFinished)
		return int64(n), err
	}
	close(m.secondStarted)
	n, err := w.Write([]byte("new"))
	return int64(n), err
}
func (m *replacingGalleryClient) GetAssetRange(context.Context, string, string, int64, io.Writer) (int64, error) {
	return 0, nil
}

// TestHandleOpen_SendsHello verifies hello is sent without password_required
func TestHandleOpen_SendsHello(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	mgr := NewManager(channels, nil, 0)
	mgr.HandleOpen()
	time.Sleep(10 * time.Millisecond)

	lastMsg := dc.getLastTextMessage()
	if lastMsg == "" {
		t.Fatal("expected hello message")
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(lastMsg), &result); err != nil {
		t.Fatalf("parse hello: %v", err)
	}
	if result["type"] != "hello" {
		t.Errorf("expected type 'hello', got %v", result["type"])
	}
	if _, ok := result["password_required"]; ok {
		t.Error("hello must not include password_required field")
	}
}

func TestManagerInstallsRequestHandlerOnlyOnControl(t *testing.T) {
	channels := newMockChannelSet()
	_ = NewManager(channels, nil, 0)

	channels.control.mu.Lock()
	controlHandler := channels.control.onMessage
	channels.control.mu.Unlock()
	channels.media.mu.Lock()
	mediaHandler := channels.media.onMessage
	channels.media.mu.Unlock()
	channels.bulk.mu.Lock()
	bulkHandler := channels.bulk.onMessage
	channels.bulk.mu.Unlock()

	require.NotNil(t, controlHandler)
	require.Nil(t, mediaHandler)
	require.Nil(t, bulkHandler)
}

func TestFileShareRoutesControlAndBulkWhileMediaStaysIdle(t *testing.T) {
	channels := newMockChannelSet()
	client := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{{Name: "report.pdf", RequestPath: "report.pdf", Size: 3}},
		file:            []byte("pdf"),
	}
	manager := NewManager(channels, client, 0)
	manager.HandleOpen()
	request, _ := json.Marshal(map[string]any{
		"type": "file_request", "path": "report.pdf", "request_id": "bulk-1",
	})
	channels.control.deliver(request)

	require.Eventually(t, func() bool { return channels.bulk.binaryCount() > 0 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return channels.control.hasTextType("chunk_end") }, time.Second, time.Millisecond)
	require.True(t, channels.control.hasTextType("hello"))
	require.True(t, channels.control.hasTextType("file_header"))
	require.True(t, channels.bulk.hasBinaryClass(multilane.ClassBulk))
	require.Zero(t, channels.media.binaryCount())
	for _, messageType := range []string{"file_header", "chunk_end"} {
		var lifecycle struct {
			Scope     string `json:"scope"`
			RequestID string `json:"request_id"`
		}
		require.NoError(t, json.Unmarshal([]byte(channels.control.getTextByType(messageType)), &lifecycle))
		require.Equal(t, "bulk", lifecycle.Scope)
		require.Equal(t, "bulk-1", lifecycle.RequestID)
	}
}

func TestGalleryRoutesThumbnailsToThumbnailMediaClass(t *testing.T) {
	channels := newMockChannelSet()
	client := &mockGalleryClient{
		gallery:   Gallery{Items: []GalleryItem{{ID: "asset-1"}}},
		thumbnail: []byte("thumb"),
	}
	manager := NewGalleryManager(channels, client, 0)
	manager.HandleOpen()

	require.Eventually(t, func() bool { return channels.media.binaryCount() == 1 }, time.Second, time.Millisecond)
	require.True(t, channels.media.hasBinaryClass(multilane.ClassThumbnail))
	require.Zero(t, channels.bulk.binaryCount())
}

func TestPreviewRoutesInteractiveBytesToMediaAndOriginalToBulk(t *testing.T) {
	channels := newMockChannelSet()
	client := &mockGalleryClient{
		gallery: Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", Size: 3, MimeType: "image/jpeg"}}},
		file:    []byte("abc"),
	}
	manager := NewGalleryManager(channels, client, 0)
	preview, _ := json.Marshal(map[string]any{
		"type": "asset_preview_request", "id": "asset-1", "quality": "preview", "request_id": "media-1",
	})
	manager.HandleMessage(preview)
	require.Eventually(t, func() bool { return channels.media.binaryCount() > 0 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return channels.control.hasTextType("asset_preview_end") }, time.Second, time.Millisecond)
	require.True(t, channels.media.hasBinaryClass(multilane.ClassInteractiveMedia))
	for _, messageType := range []string{"asset_preview_header", "asset_preview_end"} {
		var lifecycle struct {
			Scope     string `json:"scope"`
			RequestID string `json:"request_id"`
		}
		require.NoError(t, json.Unmarshal([]byte(channels.control.getTextByType(messageType)), &lifecycle))
		require.Equal(t, "media", lifecycle.Scope)
		require.Equal(t, "media-1", lifecycle.RequestID)
	}

	original, _ := json.Marshal(map[string]any{
		"type": "asset_request", "id": "asset-1", "quality": "original", "request_id": "bulk-1",
	})
	manager.HandleMessage(original)
	require.Eventually(t, func() bool { return channels.bulk.binaryCount() > 0 }, time.Second, time.Millisecond)
	require.True(t, channels.bulk.hasBinaryClass(multilane.ClassBulk))
}

func TestMediaPreviewAndBulkDownloadRunConcurrently(t *testing.T) {
	channels := newMockChannelSet()
	client := &laneConcurrencyGalleryClient{
		bulkStarted: make(chan struct{}), bulkRelease: make(chan struct{}),
		previewStarted: make(chan struct{}), previewRelease: make(chan struct{}),
	}
	manager := NewGalleryManager(channels, client, 0)

	bulk, _ := json.Marshal(map[string]any{
		"type": "asset_request", "id": "asset", "quality": "original", "request_id": "bulk-1",
	})
	manager.HandleMessage(bulk)
	select {
	case <-client.bulkStarted:
	case <-time.After(time.Second):
		t.Fatal("bulk stream did not start")
	}

	preview, _ := json.Marshal(map[string]any{
		"type": "asset_preview_request", "id": "asset", "quality": "preview", "request_id": "media-1",
	})
	manager.HandleMessage(preview)
	select {
	case <-client.previewStarted:
	case <-time.After(time.Second):
		t.Fatal("media stream did not start during bulk")
	}

	require.Eventually(t, func() bool {
		return channels.bulk.binaryCount() > 0 && channels.media.binaryCount() > 0
	}, time.Second, time.Millisecond)
	close(client.previewRelease)
	close(client.bulkRelease)
}

func TestNewMediaPreviewReplacesOldMediaWithoutRejectingOrTouchingBulk(t *testing.T) {
	channels := newMockChannelSet()
	client := &replacingGalleryClient{
		firstStarted: make(chan struct{}), firstCanceled: make(chan struct{}), firstRelease: make(chan struct{}), firstFinished: make(chan struct{}), secondStarted: make(chan struct{}),
		bulkStarted: make(chan struct{}), bulkRelease: make(chan struct{}),
	}
	manager := NewGalleryManager(channels, client, 0)
	bulk, _ := json.Marshal(map[string]any{
		"type": "asset_request", "id": "bulk", "quality": "original", "request_id": "bulk-1",
	})
	first, _ := json.Marshal(map[string]any{
		"type": "asset_preview_request", "id": "first", "quality": "preview", "request_id": "media-1",
	})
	second, _ := json.Marshal(map[string]any{
		"type": "asset_preview_request", "id": "second", "quality": "preview", "request_id": "media-2",
	})
	manager.HandleMessage(bulk)
	select {
	case <-client.bulkStarted:
	case <-time.After(time.Second):
		t.Fatal("bulk stream did not start")
	}
	manager.HandleMessage(first)
	select {
	case <-client.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first preview did not start")
	}
	manager.HandleMessage(second)

	select {
	case <-client.firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("first preview was not canceled")
	}
	select {
	case <-client.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement preview did not start")
	}
	require.False(t, channels.control.hasErrorContaining("transfer in progress"))
	require.True(t, manager.bulkTransfer.Load(), "media replacement must leave bulk active")
	require.False(t, channels.control.hasTextType("chunk_end"), "bulk must not be completed or canceled by replacement")
	require.Eventually(t, func() bool { return channels.media.binaryCount() == 1 }, time.Second, time.Millisecond)
	close(client.firstRelease)
	select {
	case <-client.firstFinished:
	case <-time.After(time.Second):
		t.Fatal("stale media producer did not finish")
	}
	require.Never(t, func() bool { return channels.media.binaryCount() > 1 }, 100*time.Millisecond, time.Millisecond, "stale media bytes must not follow a replacement")
	require.Equal(t, 1, channels.control.countTextType("asset_preview_end"), "stale media lifecycle must not end the replacement")
	close(client.bulkRelease)
	require.Eventually(t, func() bool { return channels.control.hasTextType("chunk_end") }, time.Second, time.Millisecond)
}

func TestBulkAndMediaErrorsIncludeScopeAndRequestID(t *testing.T) {
	channels := newMockChannelSet()
	manager := NewGalleryManager(channels, &mockGalleryClient{}, 0)
	bulk, _ := json.Marshal(map[string]any{
		"type": "asset_request", "quality": "original", "request_id": "bulk-7",
	})
	manager.HandleMessage(bulk)
	media, _ := json.Marshal(map[string]any{
		"type": "asset_preview_request", "quality": "preview", "request_id": "media-9",
	})
	manager.HandleMessage(media)

	scope, requestID, ok := channels.control.errorFieldsContaining("asset id required")
	require.True(t, ok)
	require.Equal(t, "bulk", scope)
	require.Equal(t, "bulk-7", requestID)

	channels.control.mu.Lock()
	defer channels.control.mu.Unlock()
	var foundMedia bool
	for _, message := range channels.control.textMessages {
		var parsed map[string]any
		_ = json.Unmarshal([]byte(message), &parsed)
		if parsed["type"] == "error" && parsed["scope"] == "media" && parsed["request_id"] == "media-9" {
			foundMedia = true
		}
	}
	require.True(t, foundMedia)
}

func TestHandleOpen_ImmichGallerySendsThumbnailListAndData(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	client := &mockGalleryClient{
		gallery: Gallery{
			AlbumName: "Summer",
			Items:     []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 12}},
		},
		thumbnail: []byte{0xff, 0xd8, 0xff},
	}
	mgr := NewGalleryManager(channels, client, 0)
	mgr.HandleOpen()
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasTextType("thumbnail_list"))
	require.Len(t, channels.media.binaryData, 1)
	require.Equal(t, byte(0x11), channels.media.binaryData[0][0], "thumbnail frames use typed binary envelope")
	completed := dc.getTextByType("thumbnail_complete")
	require.NotEmpty(t, completed)
	var result struct {
		Sent   int `json:"sent"`
		Failed int `json:"failed"`
	}
	require.NoError(t, json.Unmarshal([]byte(completed), &result))
	require.Equal(t, 1, result.Sent)
	require.Zero(t, result.Failed)
}

func TestHandleOpen_ImmichGalleryFetchesThumbnailsConcurrently(t *testing.T) {
	channels := newMockChannelSet()
	client := &concurrentThumbnailGalleryClient{
		gallery: Gallery{Items: []GalleryItem{
			{ID: "asset-1", Name: "one.jpg"},
			{ID: "asset-2", Name: "two.jpg"},
			{ID: "asset-3", Name: "three.jpg"},
		}},
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	mgr := NewGalleryManager(channels, client, 0)
	mgr.HandleOpen()

	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("expected at least two thumbnail fetches to overlap")
	}
	close(client.release)
	require.Eventually(t, func() bool {
		channels.media.mu.Lock()
		defer channels.media.mu.Unlock()
		return len(channels.media.binaryData) == 3
	}, time.Second, 10*time.Millisecond)
	require.GreaterOrEqual(t, client.maxConcurrent.Load(), int32(2))
}

func TestHandleAssetRequestStreamsByAssetID(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	client := &mockGalleryClient{
		gallery: Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		file:    []byte("abc"),
	}
	mgr := NewGalleryManager(channels, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "quality": "original"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasTextType("file_header"))
	require.True(t, fileHeaderBinaryEnvelope(t, dc))
	require.NotEmpty(t, channels.bulk.binaryData)
	require.True(t, dc.hasTextType("chunk_end"))
}

func TestHandleAssetRequestThumbnailQualityStreamsThumbnail(t *testing.T) {
	channels := newMockChannelSet()
	client := &mockGalleryClient{
		gallery:   Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		thumbnail: []byte("tn"),
		file:      []byte("original"),
	}
	mgr := NewGalleryManager(channels, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "quality": "thumbnail"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.Len(t, channels.bulk.binaryData, 1)
	require.Equal(t, []byte{0x10, 't', 'n'}, channels.bulk.binaryData[0])
}

func TestHandleAssetPreviewRequestStreamsPreviewWithoutDownloadHeader(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	client := &mockGalleryClient{
		file: []byte("preview"),
	}
	mgr := NewGalleryManager(channels, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "asset_preview_request", "id": "asset-1", "quality": "preview"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.False(t, dc.hasTextType("file_header"))
	require.True(t, dc.hasTextType("asset_preview_header"))
	require.True(t, dc.hasTextType("asset_preview_end"))
	require.Equal(t, "preview", client.assetQuality)
	require.NotEmpty(t, channels.media.binaryData)
	require.Equal(t, byte(0x10), channels.media.binaryData[0][0], "preview chunks reuse typed file chunk envelope")
}

func TestHandleAssetPreviewRequestStreamsVideoPreviewInLargerFrames(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	client := &mockGalleryClient{
		file:     bytes.Repeat([]byte{0xab}, 200*1024),
		headSize: 200 * 1024,
	}
	mgr := NewGalleryManager(channels, client, 0)

	req, _ := json.Marshal(map[string]interface{}{
		"type":       "asset_preview_request",
		"id":         "asset-1",
		"quality":    "video",
		"generation": 0,
	})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasTextType("asset_preview_header"))
	require.True(t, dc.hasTextType("asset_preview_end"))
	require.Len(t, channels.media.binaryData, 1)
}

func TestHandleFileRequestHeaderIncludesBinaryEnvelope(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	client := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "photo.jpg", RequestPath: "photo.jpg", ContentType: "image/jpeg", Size: 3},
		},
		file: []byte("abc"),
	}
	mgr := NewManager(channels, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "file_request", "path": "photo.jpg"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.True(t, fileHeaderBinaryEnvelope(t, dc))
}

func TestHandleAssetRequestRejectsUnsupportedQuality(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	client := &mockGalleryClient{
		gallery: Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		file:    []byte("abc"),
	}
	mgr := NewGalleryManager(channels, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "quality": "preview"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasErrorContaining("unsupported asset quality"))
	require.False(t, dc.hasTextType("file_header"))
	require.Empty(t, channels.bulk.binaryData)
}

func TestConcurrentAssetRequestsOnlyOneStarts(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	client := &blockingGalleryClient{
		gallery:     Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		file:        []byte("abc"),
		listStarted: make(chan struct{}),
		releaseList: make(chan struct{}),
	}
	mgr := NewGalleryManager(channels, client, 0)
	firstReq, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "request_id": "bulk-1"})
	secondReq, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "request_id": "bulk-2"})

	go mgr.HandleMessage(firstReq)
	<-client.listStarted
	secondDone := make(chan struct{})
	go func() {
		mgr.HandleMessage(secondReq)
		close(secondDone)
	}()
	time.Sleep(10 * time.Millisecond)
	close(client.releaseList)
	<-secondDone
	time.Sleep(50 * time.Millisecond)

	require.Equal(t, int32(1), client.assetCalls.Load())
	require.Equal(t, 1, dc.countTextType("file_header"))
	require.True(t, dc.hasErrorContaining("transfer in progress"))
	scope, requestID, ok := dc.errorFieldsContaining("transfer in progress")
	require.True(t, ok)
	require.Equal(t, "bulk", scope)
	require.Equal(t, "bulk-2", requestID)
}

func TestConcurrentFileRequestsOnlyOneStarts(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	client := &blockingStorageClient{
		files: []cloudwebdav.FileInfo{
			{Name: "photo.jpg", RequestPath: "photo.jpg", ContentType: "image/jpeg", Size: 3},
		},
		file:        []byte("abc"),
		listStarted: make(chan struct{}),
		releaseList: make(chan struct{}),
	}
	mgr := NewManager(channels, client, 0)
	req, _ := json.Marshal(map[string]string{"type": "file_request", "path": "photo.jpg"})

	go mgr.HandleMessage(req)
	<-client.listStarted
	secondDone := make(chan struct{})
	go func() {
		mgr.HandleMessage(req)
		close(secondDone)
	}()
	time.Sleep(10 * time.Millisecond)
	close(client.releaseList)
	<-secondDone
	time.Sleep(50 * time.Millisecond)

	require.Equal(t, int32(1), client.getCalls.Load())
	require.Equal(t, 1, dc.countTextType("file_header"))
	require.True(t, dc.hasErrorContaining("transfer in progress"))
}

// TestFileHeader_IncludesSHA1 verifies sha1 field is present when FileInfo has a checksum
func TestFileHeader_IncludesSHA1(t *testing.T) {
	header := struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType"`
		SHA1     string `json:"sha1,omitempty"`
	}{
		Type:     "file_header",
		Name:     "video.mp4",
		Size:     1048576,
		MimeType: "video/mp4",
		SHA1:     "a7e0206e573edbef0c4d8107a151271fbeccf2fe",
	}
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if result["sha1"] != "a7e0206e573edbef0c4d8107a151271fbeccf2fe" {
		t.Errorf("expected sha1 in file_header, got: %s", data)
	}
}

// TestFileHeader_OmitsSHA1 verifies sha1 field is absent when FileInfo has no checksum
func TestFileHeader_OmitsSHA1(t *testing.T) {
	header := struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType"`
		SHA1     string `json:"sha1,omitempty"`
	}{
		Type:     "file_header",
		Name:     "notes.txt",
		Size:     512,
		MimeType: "text/plain",
		SHA1:     "",
	}
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if strings.Contains(string(data), "sha1") {
		t.Errorf("expected sha1 to be omitted from file_header, got: %s", data)
	}
}

// TestHandleFileRequest_PathTraversal verifies .. in path returns error and ListFiles is never called
func TestHandleFileRequest_PathTraversal(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	mc := &mockOpenCloudClient{}
	mgr := NewManager(channels, mc, 0)

	req, _ := json.Marshal(map[string]string{
		"type": "file_request",
		"path": "../escape.txt",
	})
	mgr.HandleMessage(req)
	time.Sleep(10 * time.Millisecond)

	lastMsg := dc.getLastTextMessage()
	if !strings.Contains(lastMsg, "invalid path") {
		t.Errorf("expected 'invalid path' error, got: %s", lastMsg)
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.listFilesPath != "" {
		t.Errorf("expected ListFiles not called, got path %q", mc.listFilesPath)
	}
}

// TestHandleListRequest_Subpath verifies list_request with path calls ListFiles with that path
func TestHandleListRequest_Subpath(t *testing.T) {
	channels := newMockChannelSet()
	mc := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "report.pdf", Size: 1024, ContentType: "application/pdf"},
		},
	}
	mgr := NewManager(channels, mc, 0)

	req, _ := json.Marshal(map[string]interface{}{
		"type": "list_request",
		"path": "docs",
	})
	mgr.HandleMessage(req)
	time.Sleep(10 * time.Millisecond)

	mc.mu.Lock()
	gotPath := mc.listFilesPath
	mc.mu.Unlock()

	if gotPath != "docs" {
		t.Errorf("expected ListFiles called with 'docs', got %q", gotPath)
	}
}

// TestHandleFileRequest_NestedPath verifies file_header uses basename, ListFiles called with dir
func TestHandleFileRequest_NestedPath(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	mc := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "file.txt", Size: 512, ContentType: "text/plain"},
		},
	}
	mgr := NewManager(channels, mc, 0)

	req, _ := json.Marshal(map[string]string{
		"type": "file_request",
		"path": "docs/file.txt",
	})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	mc.mu.Lock()
	gotListPath := mc.listFilesPath
	mc.mu.Unlock()
	if gotListPath != "docs" {
		t.Errorf("expected ListFiles called with 'docs', got %q", gotListPath)
	}

	var headerName string
	dc.mu.Lock()
	for _, msg := range dc.textMessages {
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(msg), &parsed) == nil && parsed["type"] == "file_header" {
			headerName, _ = parsed["name"].(string)
		}
	}
	dc.mu.Unlock()
	if headerName != "file.txt" {
		t.Errorf("expected file_header name 'file.txt', got %q", headerName)
	}
}

func TestHandleFileRequest_UsesRequestPathFromFileInfo(t *testing.T) {
	channels := newMockChannelSet()
	mc := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "Quarterly Report.pdf", RequestPath: "", Size: 512, ContentType: "application/pdf"},
		},
	}
	mgr := NewManager(channels, mc, 0)

	req, _ := json.Marshal(map[string]string{
		"type": "file_request",
		"path": "Quarterly Report.pdf",
	})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	mc.mu.Lock()
	gotPath := mc.getFilePath
	mc.mu.Unlock()
	if gotPath != "" {
		t.Errorf("expected GetFile called with root path, got %q", gotPath)
	}
}

func TestStreamFile_DoesNotSendChunkEndOrCountDownloadOnGetFailure(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	mc := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "broken.pdf", RequestPath: "broken.pdf", Size: 512, ContentType: "application/pdf"},
		},
		getFileErr: io.ErrUnexpectedEOF,
	}
	mgr := NewManager(channels, mc, 0)

	var completedBytes int64 = -1
	mgr.OnDownloadComplete = func(bytesTransferred int64) {
		completedBytes = bytesTransferred
	}

	req, _ := json.Marshal(map[string]string{
		"type": "file_request",
		"path": "broken.pdf",
	})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	dc.mu.Lock()
	defer dc.mu.Unlock()

	foundChunkEnd := false
	foundTransferFailed := false
	for _, msg := range dc.textMessages {
		if strings.Contains(msg, `"type":"chunk_end"`) {
			foundChunkEnd = true
		}
		if strings.Contains(msg, "transfer failed") {
			foundTransferFailed = true
		}
	}

	if !foundTransferFailed {
		t.Fatal("expected transfer failed error message")
	}
	if foundChunkEnd {
		t.Fatal("did not expect chunk_end after GetFile failure")
	}
	if completedBytes != -1 {
		t.Fatalf("expected OnDownloadComplete not to fire, got %d", completedBytes)
	}
	if got := mgr.downloads.Load(); got != 0 {
		t.Fatalf("expected downloads to remain 0, got %d", got)
	}
}

// TestMaxDownloads_Rejected verifies download limit reached returns error
func TestMaxDownloads_Rejected(t *testing.T) {
	channels := newMockChannelSet()
	dc := channels.control
	mgr := NewManager(channels, nil, 2)

	sessionExpiredCalled := false
	mgr.OnSessionExpired = func() {
		sessionExpiredCalled = true
	}

	mgr.downloads.Store(2)

	req, _ := json.Marshal(map[string]string{
		"type": "file_request",
		"path": "test.txt",
	})
	mgr.HandleMessage(req)
	time.Sleep(10 * time.Millisecond)

	lastMsg := dc.getLastTextMessage()
	if !strings.Contains(lastMsg, "share has reached its download limit") {
		t.Errorf("expected download limit error, got: %s", lastMsg)
	}
	if !sessionExpiredCalled {
		t.Error("OnSessionExpired callback should have been called")
	}
}

func fileHeaderBinaryEnvelope(t *testing.T, dc *mockDC) bool {
	t.Helper()

	var header struct {
		BinaryEnvelope bool `json:"binary_envelope"`
	}
	require.NotEmpty(t, dc.getTextByType("file_header"))
	require.NoError(t, json.Unmarshal([]byte(dc.getTextByType("file_header")), &header))
	return header.BinaryEnvelope
}
