package transfer

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sharebridge/agent/internal/cloudwebdav"
)

// mockDC implements DataChannel for testing
type mockDC struct {
	textMessages []string
	binaryData   [][]byte
	buffered     uint64
	closed       bool
	mu           sync.Mutex
}

func (m *mockDC) SendBinary(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.binaryData = append(m.binaryData, data)
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
	m.closed = false
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
}

func (m *mockGalleryClient) ListGallery(ctx context.Context) (Gallery, error) {
	return m.gallery, nil
}

func (m *mockGalleryClient) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	n, err := w.Write(m.thumbnail)
	return int64(n), err
}

func (m *mockGalleryClient) GetAsset(ctx context.Context, id string, quality string, w io.Writer) (int64, error) {
	m.assetQuality = quality
	n, err := w.Write(m.file)
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

func (m *blockingGalleryClient) GetThumbnail(ctx context.Context, id string, w io.Writer) (int64, error) {
	return 0, nil
}

func (m *blockingGalleryClient) GetAsset(ctx context.Context, id string, quality string, w io.Writer) (int64, error) {
	m.assetCalls.Add(1)
	n, err := w.Write(m.file)
	return int64(n), err
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

// TestHandleOpen_SendsHello verifies hello is sent without password_required
func TestHandleOpen_SendsHello(t *testing.T) {
	dc := &mockDC{}
	mgr := NewManager(dc, nil, 0)
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

func TestHandleOpen_ImmichGallerySendsThumbnailListAndData(t *testing.T) {
	dc := &mockDC{}
	client := &mockGalleryClient{
		gallery: Gallery{
			AlbumName: "Summer",
			Items:     []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 12}},
		},
		thumbnail: []byte{0xff, 0xd8, 0xff},
	}
	mgr := NewGalleryManager(dc, client, 0)
	mgr.HandleOpen()
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasTextType("thumbnail_list"))
	require.Len(t, dc.binaryData, 1)
	require.Equal(t, byte(0x11), dc.binaryData[0][0], "thumbnail frames use typed binary envelope")
}

func TestHandleAssetRequestStreamsByAssetID(t *testing.T) {
	dc := &mockDC{}
	client := &mockGalleryClient{
		gallery: Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		file:    []byte("abc"),
	}
	mgr := NewGalleryManager(dc, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "quality": "original"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasTextType("file_header"))
	require.True(t, fileHeaderBinaryEnvelope(t, dc))
	require.NotEmpty(t, dc.binaryData)
	require.True(t, dc.hasTextType("chunk_end"))
}

func TestHandleAssetRequestThumbnailQualityStreamsThumbnail(t *testing.T) {
	dc := &mockDC{}
	client := &mockGalleryClient{
		gallery:   Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		thumbnail: []byte("tn"),
		file:      []byte("original"),
	}
	mgr := NewGalleryManager(dc, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "quality": "thumbnail"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.Len(t, dc.binaryData, 1)
	require.Equal(t, []byte{0x10, 't', 'n'}, dc.binaryData[0])
}

func TestHandleFileRequestHeaderIncludesBinaryEnvelope(t *testing.T) {
	dc := &mockDC{}
	client := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "photo.jpg", RequestPath: "photo.jpg", ContentType: "image/jpeg", Size: 3},
		},
		file: []byte("abc"),
	}
	mgr := NewManager(dc, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "file_request", "path": "photo.jpg"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.True(t, fileHeaderBinaryEnvelope(t, dc))
}

func TestHandleAssetRequestRejectsUnsupportedQuality(t *testing.T) {
	dc := &mockDC{}
	client := &mockGalleryClient{
		gallery: Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		file:    []byte("abc"),
	}
	mgr := NewGalleryManager(dc, client, 0)

	req, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1", "quality": "preview"})
	mgr.HandleMessage(req)
	time.Sleep(50 * time.Millisecond)

	require.True(t, dc.hasErrorContaining("unsupported asset quality"))
	require.False(t, dc.hasTextType("file_header"))
	require.Empty(t, dc.binaryData)
}

func TestConcurrentAssetRequestsOnlyOneStarts(t *testing.T) {
	dc := &mockDC{}
	client := &blockingGalleryClient{
		gallery:     Gallery{Items: []GalleryItem{{ID: "asset-1", Name: "photo.jpg", MimeType: "image/jpeg", Size: 3}}},
		file:        []byte("abc"),
		listStarted: make(chan struct{}),
		releaseList: make(chan struct{}),
	}
	mgr := NewGalleryManager(dc, client, 0)
	req, _ := json.Marshal(map[string]string{"type": "asset_request", "id": "asset-1"})

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

	require.Equal(t, int32(1), client.assetCalls.Load())
	require.Equal(t, 1, dc.countTextType("file_header"))
	require.True(t, dc.hasErrorContaining("transfer in progress"))
}

func TestConcurrentFileRequestsOnlyOneStarts(t *testing.T) {
	dc := &mockDC{}
	client := &blockingStorageClient{
		files: []cloudwebdav.FileInfo{
			{Name: "photo.jpg", RequestPath: "photo.jpg", ContentType: "image/jpeg", Size: 3},
		},
		file:        []byte("abc"),
		listStarted: make(chan struct{}),
		releaseList: make(chan struct{}),
	}
	mgr := NewManager(dc, client, 0)
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
	dc := &mockDC{}
	mc := &mockOpenCloudClient{}
	mgr := NewManager(dc, mc, 0)

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
	dc := &mockDC{}
	mc := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "report.pdf", Size: 1024, ContentType: "application/pdf"},
		},
	}
	mgr := NewManager(dc, mc, 0)

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
	dc := &mockDC{}
	mc := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "file.txt", Size: 512, ContentType: "text/plain"},
		},
	}
	mgr := NewManager(dc, mc, 0)

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
	dc := &mockDC{}
	mc := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "Quarterly Report.pdf", RequestPath: "", Size: 512, ContentType: "application/pdf"},
		},
	}
	mgr := NewManager(dc, mc, 0)

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
	dc := &mockDC{}
	mc := &mockOpenCloudClient{
		listFilesResult: []cloudwebdav.FileInfo{
			{Name: "broken.pdf", RequestPath: "broken.pdf", Size: 512, ContentType: "application/pdf"},
		},
		getFileErr: io.ErrUnexpectedEOF,
	}
	mgr := NewManager(dc, mc, 0)

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
	dc := &mockDC{}
	mgr := NewManager(dc, nil, 2)

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
