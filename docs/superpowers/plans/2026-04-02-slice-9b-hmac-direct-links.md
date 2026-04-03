# Slice 9b — HMAC Pre-challenge + Direct Links Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent unauthenticated browsers from triggering WebRTC peer creation (and IP exposure) by requiring an HMAC proof-of-password before the agent creates any peer, and add direct-link support so recipients can click a URL instead of typing a session code.

**Architecture:** The browser knocks to get a per-connection nonce from the agent (via the signaling server), computes `HMAC-SHA256(key=password, data=nonce)`, and sends that in the join message. The agent verifies the HMAC before creating a WebRTC peer — no IP leak, no resource exhaustion from unauthenticated joins. Direct links work by serving the existing `index.html` at `/s/:code`, reading the code from the URL path, and optionally reading the password from the URL hash (cleared immediately via `history.replaceState`).

**Tech Stack:** Go (agent + signaling server), vanilla JS (browser), `crypto/hmac` + `crypto/sha256` (Go), Web Crypto API `HMAC-SHA256` (browser), Gin (routing)

---

## File Map

| File | Change |
|------|--------|
| `agent/internal/transfer/manager.go` | Remove password/auth fields, simplify `NewManager`, remove `HandleOpen` password_required |
| `agent/internal/transfer/manager_test.go` | Delete auth tests, add `TestHandleOpen_SendsHello`, update `TestHandleListRequest_Subpath` |
| `agent/internal/signaling/client.go` | Add `ConnID`, `HMAC` fields to `Message` struct |
| `agent/internal/daemon/daemon.go` | Add nonce store, `handleKnock`, `handleJoin`, rename `handleBrowserJoin` → `createPeer`; remove `OnAuthFailed` wiring; update `NewManager` call |
| `signaling-server/internal/hub/hub.go` | Add `connBrowsers`/`connFails` maps; add `RegisterBrowserConn`, `UnregisterBrowserConn`, `ForwardToBrowserByConnID`, `IncrementAuthFailure`, `CloseBrowserConnWithError` |
| `signaling-server/internal/handler/agent_ws.go` | Add `ConnID`, `Value`, `HasPassword` to `agentMsg`; handle `nonce` (route to browser by connID); change `auth_failed` (track + close browser after 3 failures) |
| `signaling-server/internal/handler/browser_ws.go` | Generate `connID` on connect; register in hub; delay agent notification; add `HMAC` to `browserMsg`; handle knock/join from browser; add `peer_id` to forwarded answer/ice |
| `signaling-server/internal/handler/routes.go` | Add `GET /s/:code` serving `index.html` |
| `signaling-server/web/app.js` | Rewrite join/auth flow: knock→nonce→HMAC→join; remove DataChannel password logic; add URL path/hash parsing for direct links |

---

## Task 1: Clean up transfer/Manager — remove DataChannel auth

**Files:**
- Modify: `agent/internal/transfer/manager.go`
- Modify: `agent/internal/transfer/manager_test.go`

- [ ] **Step 1: Write the failing test for the new `HandleOpen` (no `password_required` field)**

```go
// Add to agent/internal/transfer/manager_test.go
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
```

- [ ] **Step 2: Run the test to confirm it fails**

```bash
cd agent && go test ./internal/transfer/... -run TestHandleOpen_SendsHello -v
```

Expected: FAIL — `NewManager` currently takes 4 args, compilation error.

- [ ] **Step 3: Replace `manager.go` with the simplified version**

Replace the entire file `agent/internal/transfer/manager.go`:

```go
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
	ListFiles(subpath string) ([]opencloud.FileInfo, error)
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
		Type  string               `json:"type"`
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
```

- [ ] **Step 4: Update `manager_test.go` — delete auth tests, update subpath test, add hello test**

Replace the entire file `agent/internal/transfer/manager_test.go`:

```go
package transfer

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"opencloudshare/agent/internal/opencloud"
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

// mockOpenCloudClient implements openCloudClient for testing
type mockOpenCloudClient struct {
	mu              sync.Mutex
	listFilesPath   string
	listFilesResult []opencloud.FileInfo
	listFilesErr    error
	getFilePath     string
}

func (m *mockOpenCloudClient) ListFiles(subpath string) ([]opencloud.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listFilesPath = subpath
	return m.listFilesResult, m.listFilesErr
}

func (m *mockOpenCloudClient) GetFile(filePath string, w io.Writer) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getFilePath = filePath
	return 0, nil
}

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
		listFilesResult: []opencloud.FileInfo{
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
		listFilesResult: []opencloud.FileInfo{
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
```

- [ ] **Step 5: Run all transfer tests**

```bash
cd agent && go test ./internal/transfer/... -v
```

Expected: all tests PASS. Specifically `TestHandleOpen_SendsHello`, `TestHandleFileRequest_PathTraversal`, `TestHandleListRequest_Subpath`, `TestHandleFileRequest_NestedPath`, `TestMaxDownloads_Rejected`, `TestFileHeader_IncludesSHA1`, `TestFileHeader_OmitsSHA1`.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/transfer/manager.go agent/internal/transfer/manager_test.go
git commit -m "feat(agent): remove DataChannel auth — HMAC pre-challenge replaces it"
```

---

## Task 2: Extend protocol message structs

**Files:**
- Modify: `agent/internal/signaling/client.go`
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/browser_ws.go`

- [ ] **Step 1: Add `ConnID` and `HMAC` to agent's `Message` struct**

In `agent/internal/signaling/client.go`, update the `Message` struct:

```go
// Message is any message received from the signaling server.
type Message struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"session_id,omitempty"`
	PeerID      string          `json:"peer_id,omitempty"`
	SDP         string          `json:"sdp,omitempty"`
	Candidate   json.RawMessage `json:"candidate,omitempty"`
	Err         string          `json:"message,omitempty"`
	Code        string          `json:"code,omitempty"`
	ConnID      string          `json:"conn_id,omitempty"`
	HMAC        string          `json:"hmac,omitempty"`
	Reconnected bool            `json:"reconnected,omitempty"`
	ICEServers  []ICEServer     `json:"ice_servers,omitempty"`
}
```

- [ ] **Step 2: Add `ConnID`, `Value`, `HasPassword` to `agentMsg` in `agent_ws.go`**

In `signaling-server/internal/handler/agent_ws.go`, update the `agentMsg` struct:

```go
type agentMsg struct {
	Type         string          `json:"type"`
	AgentID      string          `json:"agent_id,omitempty"`
	Code         string          `json:"code,omitempty"`
	ShareURL     string          `json:"share_url,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	MaxDownloads *int            `json:"max_downloads,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	SDP          string          `json:"sdp,omitempty"`
	Candidate    json.RawMessage `json:"candidate,omitempty"`
	ConnID       string          `json:"conn_id,omitempty"`
	Value        string          `json:"value,omitempty"`
	HasPassword  bool            `json:"has_password,omitempty"`
}
```

- [ ] **Step 3: Add `HMAC` to `browserMsg` in `browser_ws.go`**

In `signaling-server/internal/handler/browser_ws.go`, update the `browserMsg` struct:

```go
type browserMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	HMAC      string          `json:"hmac,omitempty"`
}
```

- [ ] **Step 4: Build both services to verify no compilation errors**

```bash
cd agent && go build ./... && cd ../signaling-server && go build ./...
```

Expected: clean build, no errors.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/signaling/client.go \
        signaling-server/internal/handler/agent_ws.go \
        signaling-server/internal/handler/browser_ws.go
git commit -m "feat: extend protocol message structs for HMAC knock/join/nonce"
```

---

## Task 3: Hub — connID tracking and auth failure management

**Files:**
- Modify: `signaling-server/internal/hub/hub.go`

- [ ] **Step 1: Add `connBrowsers` and `connFails` maps to the `Hub` struct and initialise them**

In `signaling-server/internal/hub/hub.go`, update the `Hub` struct and `New()`:

```go
type Hub struct {
	mu           sync.RWMutex
	agents       map[string]*websocket.Conn // apiKey → conn
	codes        map[string]string          // code → apiKey
	pairs        map[string]*pair           // sessionID → pair
	connBrowsers map[string]*websocket.Conn // connID → browser conn
	connFails    map[string]int             // connID → auth failure count
}

func New() *Hub {
	return &Hub{
		agents:       make(map[string]*websocket.Conn),
		codes:        make(map[string]string),
		pairs:        make(map[string]*pair),
		connBrowsers: make(map[string]*websocket.Conn),
		connFails:    make(map[string]int),
	}
}
```

- [ ] **Step 2: Add the four new hub methods**

Append to `signaling-server/internal/hub/hub.go` (before the closing brace — after the existing `send` function):

```go
// RegisterBrowserConn stores a browser WebSocket connection keyed by connID.
// Called when a browser WebSocket connects, before the knock/join flow.
func (h *Hub) RegisterBrowserConn(connID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connBrowsers[connID] = conn
}

// UnregisterBrowserConn removes the browser conn and its failure count.
// Called in the browser_ws.go defer when the WebSocket closes.
func (h *Hub) UnregisterBrowserConn(connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.connBrowsers, connID)
	delete(h.connFails, connID)
}

// ForwardToBrowserByConnID sends msg to the browser identified by connID.
// Used to route nonce responses from the agent back to the correct browser.
func (h *Hub) ForwardToBrowserByConnID(ctx context.Context, connID string, msg any) error {
	h.mu.RLock()
	conn, ok := h.connBrowsers[connID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	return send(ctx, conn, msg)
}

// IncrementAuthFailure increments the failure count for connID and returns
// the new total. Used by agent_ws.go to enforce the 3-strike limit.
func (h *Hub) IncrementAuthFailure(connID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connFails[connID]++
	return h.connFails[connID]
}

// CloseBrowserConnWithError sends an error message to the browser identified by
// connID and then closes its WebSocket. Used after 3 HMAC auth failures.
func (h *Hub) CloseBrowserConnWithError(ctx context.Context, connID, message string) {
	h.mu.RLock()
	conn, ok := h.connBrowsers[connID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	send(ctx, conn, map[string]string{"type": "error", "message": message})
	conn.Close(websocket.StatusNormalClosure, message)
}
```

- [ ] **Step 3: Build signaling server**

```bash
cd signaling-server && go build ./...
```

Expected: clean build.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/internal/hub/hub.go
git commit -m "feat(hub): add connID browser tracking and auth failure management"
```

---

## Task 4: Agent nonce store + HMAC knock/join handlers

**Files:**
- Modify: `agent/internal/daemon/daemon.go`

- [ ] **Step 1: Add imports and `nonceEntry` type to `daemon.go`**

Add to the import block in `agent/internal/daemon/daemon.go`:

```go
import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	// ... existing imports unchanged ...
)
```

Add the `nonceEntry` type directly after the imports (before the `WebServer` interface):

```go
// nonceEntry holds a per-connection nonce for HMAC pre-challenge.
type nonceEntry struct {
	nonce     string
	expiresAt time.Time
}
```

- [ ] **Step 2: Add nonce store fields to `Daemon` struct and initialise in `New` and `NewWithSignaling`**

In the `Daemon` struct, add two fields after `hasTURN`:

```go
nonces   map[string]nonceEntry
noncesMu sync.Mutex
```

In `New()`, add to the return literal:

```go
nonces: make(map[string]nonceEntry),
```

In `NewWithSignaling()`, add to the return literal:

```go
nonces: make(map[string]nonceEntry),
```

- [ ] **Step 3: Add `handleKnock` method**

Add this method to `daemon.go`:

```go
// handleKnock handles a knock from a browser (via signaling server).
// It generates a per-connection nonce, stores it with a 60s TTL, and
// sends it back so the browser can compute the HMAC proof.
func (d *Daemon) handleKnock(connID, sessionCode string) {
	d.mu.RLock()
	session, ok := d.sessions[sessionCode]
	d.mu.RUnlock()
	if !ok {
		log.Printf("knock for unknown session %s", sessionCode)
		return
	}

	// Generate 32 random bytes → 64-char hex nonce
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		log.Printf("generate nonce for session %s: %v", sessionCode, err)
		return
	}
	nonce := hex.EncodeToString(nonceBytes)

	// Sweep expired nonces and store new one (under same lock)
	d.noncesMu.Lock()
	now := time.Now()
	for id, entry := range d.nonces {
		if now.After(entry.expiresAt) {
			delete(d.nonces, id)
		}
	}
	d.nonces[connID] = nonceEntry{nonce: nonce, expiresAt: now.Add(60 * time.Second)}
	d.noncesMu.Unlock()

	// Send nonce back to browser (via signaling server)
	d.signaling.Send(context.Background(), map[string]any{
		"type":         "nonce",
		"conn_id":      connID,
		"value":        nonce,
		"has_password": session.Password != "",
	})

	log.Printf("nonce sent for session %s conn %s", sessionCode, connID)
}
```

- [ ] **Step 4: Add `handleJoin` method**

Add this method to `daemon.go`:

```go
// handleJoin verifies the HMAC from the browser. If valid, creates a WebRTC
// peer. If invalid, notifies the signaling server (which tracks failures and
// closes the browser WS after 3 strikes).
func (d *Daemon) handleJoin(connID, sessionCode, receivedHMAC string) {
	d.mu.RLock()
	session, ok := d.sessions[sessionCode]
	d.mu.RUnlock()
	if !ok {
		log.Printf("join for unknown session %s", sessionCode)
		return
	}

	// Atomically delete nonce entry before verifying — prevents race where
	// two concurrent join messages both read the nonce before either deletes it.
	d.noncesMu.Lock()
	entry, found := d.nonces[connID]
	delete(d.nonces, connID)
	d.noncesMu.Unlock()

	if !found || time.Now().After(entry.expiresAt) {
		log.Printf("join with expired/missing nonce: session %s conn %s", sessionCode, connID)
		d.signaling.Send(context.Background(), map[string]any{
			"type":    "auth_failed",
			"conn_id": connID,
		})
		return
	}

	// Verify HMAC for password-protected shares. Password-less shares skip verification.
	if session.Password != "" {
		mac := hmac.New(sha256.New, []byte(session.Password))
		mac.Write([]byte(entry.nonce))
		expectedMAC := mac.Sum(nil)

		receivedBytes, err := hex.DecodeString(receivedHMAC)
		if err != nil || !hmac.Equal(expectedMAC, receivedBytes) {
			log.Printf("HMAC mismatch for session %s conn %s", sessionCode, connID)
			d.signaling.Send(context.Background(), map[string]any{
				"type":    "auth_failed",
				"conn_id": connID,
			})
			return
		}
	}

	log.Printf("HMAC verified for session %s conn %s — creating peer", sessionCode, connID)
	go d.createPeer(connID, sessionCode)
}
```

- [ ] **Step 5: Rename `handleBrowserJoin` → `createPeer` and update its signature**

Rename the existing `handleBrowserJoin(sessionCode, peerID string)` to `createPeer(connID, sessionCode string)` and update all references inside it.

The method signature change:
```go
// createPeer creates a WebRTC peer connection for a browser that has passed
// HMAC verification. connID is used as the peerID throughout the session.
func (d *Daemon) createPeer(connID, sessionCode string) {
```

Inside the method body, replace every `peerID` with `connID`. The method body is otherwise identical to the old `handleBrowserJoin`. Also update `NewManager` call — remove the `session.Password` argument and remove the `tm.OnAuthFailed` wiring block:

Change:
```go
tm := transfer.NewManager(p, session.webdavClient, session.Password, session.MaxDownloads)
```
To:
```go
tm := transfer.NewManager(p, session.webdavClient, session.MaxDownloads)
```

Remove the `tm.OnAuthFailed` block entirely:
```go
// DELETE this block:
tm.OnAuthFailed = func() {
    d.signaling.Send(context.Background(), map[string]any{
        "type":       "auth_failed",
        "session_id": sessionCode,
        "peer_id":    peerID,
    })
}
```

- [ ] **Step 6: Update `handleSignalingMessage` to dispatch knock/join**

In the `handleSignalingMessage` switch, replace the existing `case "join"` and add `case "knock"`:

```go
case "knock":
    go d.handleKnock(msg.ConnID, msg.Code)

case "join":
    go d.handleJoin(msg.ConnID, msg.Code, msg.HMAC)
```

- [ ] **Step 7: Build agent**

```bash
cd agent && go build ./...
```

Expected: clean build.

- [ ] **Step 8: Run agent tests**

```bash
cd agent && go test ./...
```

Expected: all tests PASS.

- [ ] **Step 9: Commit**

```bash
git add agent/internal/daemon/daemon.go
git commit -m "feat(agent): add HMAC pre-challenge — nonce store, handleKnock, handleJoin"
```

---

## Task 5: Refactor browser_ws.go and agent_ws.go

**Files:**
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws.go`

- [ ] **Step 1: Rewrite `browser_ws.go`**

Replace the entire file `signaling-server/internal/handler/browser_ws.go`:

```go
package handler

import (
	"encoding/hex"
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/db"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/turn"
)

type browserMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	HMAC      string          `json:"hmac,omitempty"`
}

func BrowserWS(h *hub.Hub, sessionRepo *db.SessionRepo, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionCode := c.Query("session")

		session, err := sessionRepo.GetByCode(sessionCode)
		if err != nil {
			log.Printf("browser_ws: db error looking up session: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
			return
		}
		if session == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found or expired"})
			return
		}
		if session.ExpiresAt != nil && time.Now().After(*session.ExpiresAt) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found or expired"})
			return
		}

		conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx := c.Request.Context()

		_, agentOK := h.GetAgentConn(sessionCode)
		if !agentOK {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}

		if err := h.PairSession(sessionCode, conn); err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		defer h.UnpairSession(sessionCode)

		// Generate a unique connID for this browser connection.
		// Used to correlate knock/nonce/join messages and route auth failures.
		connID := generateConnID()
		h.RegisterBrowserConn(connID, conn)
		defer h.UnregisterBrowserConn(connID)

		log.Printf("browser connected to session %s (conn %s)", sessionCode, connID)

		// Send ICE config immediately so the browser can set up RTCPeerConnection
		// while the knock/nonce round-trip happens in parallel.
		var turnCreds *turn.Credentials
		if cfg.HasTurn() {
			turnExpiry := time.Now().Add(24 * time.Hour)
			creds := turn.GenerateCredentials(cfg.TurnSecret, sessionCode, turnExpiry)
			turnCreds = &creds
		}
		iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
			STUNURL:     cfg.STUNURL,
			TurnURL:     cfg.TurnURL(),
			Credentials: turnCreds,
		})
		hub.SendDirect(ctx, conn, map[string]any{
			"type":        "ice_config",
			"ice_servers": iceServers,
		})

		// NOTE: we do NOT notify the agent here (no join message).
		// The agent is notified only after the browser passes HMAC verification
		// via the knock → nonce → join(hmac) flow.

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				log.Printf("browser disconnected from session %s (conn %s)", sessionCode, connID)
				return
			}

			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "knock":
				// Forward knock to agent with connID and session code added by server.
				h.SendToAgent(ctx, session.APIKeyID, map[string]any{
					"type":    "knock",
					"conn_id": connID,
					"code":    sessionCode,
				})

			case "join":
				// Forward join to agent with connID, code, and HMAC. Agent verifies.
				h.SendToAgent(ctx, session.APIKeyID, map[string]any{
					"type":    "join",
					"conn_id": connID,
					"code":    sessionCode,
					"hmac":    msg.HMAC,
				})

			case "answer":
				// Include connID as peer_id so agent can look up the right peer.
				h.ForwardToAgent(ctx, sessionCode, map[string]any{
					"type":       "answer",
					"session_id": sessionCode,
					"peer_id":    connID,
					"sdp":        msg.SDP,
				})

			case "ice_candidate":
				h.ForwardToAgent(ctx, sessionCode, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionCode,
					"peer_id":    connID,
					"candidate":  msg.Candidate,
				})
			}
		}
	}
}

// generateConnID returns a random 32-char hex string for use as a connection ID.
func generateConnID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
```

- [ ] **Step 2: Update `agent_ws.go` — handle `nonce` routing and updated `auth_failed` logic**

In `signaling-server/internal/handler/agent_ws.go`, replace the `nonce` and `auth_failed` cases in the main switch (and remove the old `auth_failed` log-only case):

```go
case "nonce":
    // Route nonce from agent to the specific browser identified by connID.
    if agentID == "" {
        continue
    }
    h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
        "type":         "nonce",
        "conn_id":      msg.ConnID,
        "value":        msg.Value,
        "has_password": msg.HasPassword,
    })

case "auth_failed":
    // Track failures per connID. After 3, close the browser WebSocket.
    // Browser receives auth_failed with attempts_remaining so it can re-prompt.
    if agentID == "" {
        continue
    }
    failures := h.IncrementAuthFailure(msg.ConnID)
    log.Printf("HMAC auth failed: conn %s failure %d/3", msg.ConnID, failures)
    if failures >= 3 {
        h.CloseBrowserConnWithError(ctx, msg.ConnID, "too many incorrect password attempts")
    } else {
        h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
            "type":              "auth_failed",
            "attempts_remaining": 3 - failures,
        })
    }
```

Also add `"crypto/rand"` is already imported in agent_ws.go — no import change needed. But check that `hub` package methods are available: `h.ForwardToBrowserByConnID`, `h.IncrementAuthFailure`, `h.CloseBrowserConnWithError` — these were added in Task 3.

Also update the import in `browser_ws.go` — `math/rand` is used for `generateConnID`. If the project already imports `crypto/rand` in agent_ws.go, use it instead for better randomness:

Actually use `crypto/rand` instead of `math/rand` in `generateConnID`. Update the import in `browser_ws.go`:

```go
import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"
	// ... rest unchanged
)
```

And `generateConnID` stays the same (uses `rand.Read` which works with `crypto/rand`).

- [ ] **Step 3: Build signaling server**

```bash
cd signaling-server && go build ./...
```

Expected: clean build.

- [ ] **Step 4: Commit**

```bash
git add signaling-server/internal/handler/browser_ws.go \
        signaling-server/internal/handler/agent_ws.go
git commit -m "feat(server): HMAC pre-challenge flow — knock/nonce/join routing, auth failure tracking"
```

---

## Task 6: Add /s/:code direct-link route

**Files:**
- Modify: `signaling-server/internal/handler/routes.go`

- [ ] **Step 1: Add the route**

In `signaling-server/internal/handler/routes.go`, add one route to `RegisterRoutes` after the static file declarations:

```go
// Direct-link route: /s/:code serves index.html.
// Browser JS reads the code from window.location.pathname and auto-fills it.
r.GET("/s/:code", func(c *gin.Context) {
    c.File("./web/index.html")
})
```

The full `RegisterRoutes` function after the change:

```go
func RegisterRoutes(r *gin.Engine, h *hub.Hub, cfg *config.Config, apiKeyRepo *db.APIKeyRepo, sessionRepo *db.SessionRepo) {
	r.GET("/sessions/:code", GetSessionInfo(sessionRepo, h))

	r.GET("/ws/agent", AgentWS(h, apiKeyRepo, sessionRepo, cfg))
	r.GET("/ws/client", BrowserWS(h, sessionRepo, cfg))

	admin := r.Group("/admin/api")
	admin.Use(AdminAuthMiddleware(cfg.AdminToken))
	{
		admin.POST("/keys", CreateAPIKey(apiKeyRepo))
		admin.GET("/keys", ListAPIKeys(apiKeyRepo))
		admin.DELETE("/keys/:id", RevokeAPIKey(apiKeyRepo))
	}

	r.StaticFile("/", "./web/index.html")
	r.StaticFile("/app.js", "./web/app.js")

	// Direct-link route: /s/:code serves index.html.
	// Browser JS reads the code from window.location.pathname.
	r.GET("/s/:code", func(c *gin.Context) {
		c.File("./web/index.html")
	})
}
```

- [ ] **Step 2: Build and verify**

```bash
cd signaling-server && go build ./...
```

Expected: clean build.

- [ ] **Step 3: Commit**

```bash
git add signaling-server/internal/handler/routes.go
git commit -m "feat(server): add /s/:code direct-link route serving index.html"
```

---

## Task 7: Browser JS — HMAC join flow + direct links

**Files:**
- Modify: `signaling-server/web/app.js`

This task rewrites the auth and join flow in `app.js`. Read through the changes carefully — they touch many functions.

- [ ] **Step 1: Replace the top-level state variables and `join` function**

At the top of `app.js`, update the state variables. Remove `authAttempts`. Add `pendingNonce`:

```javascript
let pc, ws, dc;
let pendingCandidates = [];
let remoteDescSet = false;

// Download state
let currentFile = null;
let receivedBytes = 0;
let fileChunks = [];
let isDownloading = false;
let transferStartTime = 0;

// Navigation state
let currentPath = [];
let sessionPassword = ''; // set from URL hash on load, or from password input

// HMAC pre-challenge state
let pendingNonce = null; // nonce received from agent, consumed on join
```

Replace the `join()` function. The WS no longer notifies the agent on connect — the browser sends `knock` after receiving ICE config:

```javascript
function join() {
  const code = document.getElementById('code').value.trim();
  if (!code) return;
  status('Connecting...');

  ws = new WebSocket(`ws://${location.host}/ws/client?session=${code}`);

  ws.onmessage = async (event) => {
    const msg = JSON.parse(event.data);

    switch (msg.type) {
      case 'ice_config':
        pc = new RTCPeerConnection({ iceServers: msg.ice_servers });

        pc.onicecandidate = (e) => {
          if (e.candidate) {
            ws.send(JSON.stringify({ type: 'ice_candidate', candidate: e.candidate.toJSON() }));
          }
        };

        pc.ondatachannel = (e) => {
          dc = e.channel;
          setupDataChannel();
        };

        pc.onconnectionstatechange = () => {
          if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
            status('Connection lost');
            resetUI();
          }
        };

        // Send knock immediately after ICE config — starts the HMAC challenge flow.
        ws.send(JSON.stringify({ type: 'knock' }));
        status('Authenticating...');
        break;

      case 'nonce':
        pendingNonce = msg.value;
        if (msg.has_password && !sessionPassword) {
          // No password available — show the input field and wait for user.
          showSection('password-section');
          document.getElementById('password-input').focus();
        } else {
          // Password already known (from URL hash) or share is public — submit immediately.
          await sendJoin();
        }
        break;

      case 'auth_failed':
        // Agent rejected the HMAC. Show error and send knock for a fresh nonce.
        const errorDiv = document.getElementById('password-error');
        const remaining = msg.attempts_remaining;
        errorDiv.textContent = `Incorrect password. ${remaining} attempt${remaining === 1 ? '' : 's'} remaining.`;
        document.getElementById('password-input').value = '';
        sessionPassword = '';
        // Knock again to get a fresh nonce for the next attempt.
        ws.send(JSON.stringify({ type: 'knock' }));
        showSection('password-section');
        document.getElementById('password-input').focus();
        break;

      case 'offer':
        if (!pc) return;
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        remoteDescSet = true;
        for (const c of pendingCandidates) {
          await pc.addIceCandidate(c);
        }
        pendingCandidates = [];
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        ws.send(JSON.stringify({ type: 'answer', sdp: answer.sdp }));
        status('Negotiating...');
        break;

      case 'ice_candidate':
        if (!pc) return;
        if (!remoteDescSet) {
          pendingCandidates.push(msg.candidate);
        } else {
          await pc.addIceCandidate(msg.candidate);
        }
        break;

      case 'error':
        status('Error: ' + msg.message);
        break;
    }
  };

  ws.onerror = () => status('WebSocket error');
  ws.onclose = () => {
    if (pc) pc.close();
    resetUI();
  };
}
```

- [ ] **Step 2: Add `computeHMAC` and `sendJoin` helpers**

Add these two functions after `join()`:

```javascript
// computeHMAC computes HMAC-SHA256(key=password, data=nonce) and returns hex.
// Uses the Web Crypto API (async). Password and nonce are UTF-8 strings.
async function computeHMAC(password, nonce) {
  const encoder = new TextEncoder();
  const key = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  );
  const signature = await crypto.subtle.sign('HMAC', key, encoder.encode(nonce));
  return Array.from(new Uint8Array(signature))
    .map(b => b.toString(16).padStart(2, '0'))
    .join('');
}

// sendJoin computes the HMAC for the pending nonce and sends the join message.
async function sendJoin() {
  if (!pendingNonce) return;
  const hmac = await computeHMAC(sessionPassword, pendingNonce);
  ws.send(JSON.stringify({ type: 'join', hmac }));
  pendingNonce = null;
}
```

- [ ] **Step 3: Update `setupDataChannel` — remove password-related logic from DataChannel open event**

Replace the `dc.onopen` handler inside `setupDataChannel()`:

```javascript
dc.onopen = () => {
  status('Connected');
  hideSection('join-section');
  hideSection('password-section');
  requestFileList('');
  setTimeout(updateConnectionStatus, 1000);
};
```

Remove the `dc.onclose` password-attempt check — replace with:

```javascript
dc.onclose = () => {
  status('Connection closed');
  resetUI();
};
```

- [ ] **Step 4: Update `submitPassword` and `requestFileList`**

Replace `submitPassword`:

```javascript
async function submitPassword() {
  sessionPassword = document.getElementById('password-input').value;
  await sendJoin();
}
```

Replace `requestFileList` — remove the `password` field:

```javascript
function requestFileList(subpath) {
  dc.send(JSON.stringify({ type: 'list_request', path: subpath }));
}
```

- [ ] **Step 5: Remove `handleHello` and `handleError` password logic**

Delete the `handleHello` function entirely (it's no longer called — DataChannel `hello` no longer carries `password_required`, and file list is requested directly from `dc.onopen`).

Update `handleError` — remove the `incorrect password` branch entirely:

```javascript
function handleError(msg) {
  status('Error: ' + msg.message);
  isDownloading = false;
}
```

In the `dc.onmessage` switch, remove the `case 'hello':` branch entirely.

- [ ] **Step 6: Update `resetUI` — remove `authAttempts`**

In `resetUI()`, remove the line `authAttempts = 0;` and add `pendingNonce = null;`:

```javascript
function resetUI() {
  showSection('join-section');
  hideSection('password-section');
  hideSection('file-list');
  document.getElementById('connection-status').classList.add('hidden');
  document.getElementById('connection-type').className = 'connection-badge';
  document.getElementById('breadcrumb').classList.add('hidden');
  document.getElementById('file-list').innerHTML = '';
  document.getElementById('password-error').textContent = '';
  document.getElementById('password-input').value = '';
  document.getElementById('password-input').disabled = false;
  document.querySelector('#password-section button').disabled = false;
  currentFile = null;
  fileChunks = [];
  isDownloading = false;
  receivedBytes = 0;
  transferStartTime = 0;
  pendingNonce = null;
  currentPath = [];
  sessionPassword = '';
}
```

- [ ] **Step 7: Add `initFromURL` for direct-link support**

Add this function at the bottom of `app.js`, before any existing `window.onload` or DOMContentLoaded handlers:

```javascript
// initFromURL reads the session code from the URL path (/s/:code) and
// optionally reads the password from the URL hash (#password).
// The hash is cleared immediately via history.replaceState so it doesn't
// persist in browser history.
function initFromURL() {
  const parts = window.location.pathname.split('/');
  // /s/ABC123 → ['', 's', 'ABC123']
  if (parts[1] === 's' && parts[2]) {
    document.getElementById('code').value = parts[2];
  }

  if (window.location.hash) {
    sessionPassword = decodeURIComponent(window.location.hash.slice(1));
    history.replaceState(null, '', window.location.pathname);
  }
}

// On page load: read URL, auto-join if code is present.
document.addEventListener('DOMContentLoaded', () => {
  initFromURL();
  if (document.getElementById('code').value) {
    join();
  }
});
```

- [ ] **Step 8: Verify the signaling server serves app.js correctly and build**

```bash
cd signaling-server && go build ./...
```

Expected: clean build.

- [ ] **Step 9: Manual smoke test**

Start the signaling server and agent locally. Test the following flows:

1. **Code-only URL**: navigate to `/s/YOURCODE` — code auto-fills, browser sends knock, agent sends nonce, if no password auto-connects, file list appears.
2. **Magic link**: navigate to `/s/YOURCODE#correctpassword` — connects without any typing.
3. **Wrong password**: enter wrong password, see "X attempts remaining", re-enter correct password, connects.
4. **Plain UI**: go to `/`, enter code manually — works as before.

- [ ] **Step 10: Commit**

```bash
git add signaling-server/web/app.js
git commit -m "feat(browser): HMAC knock/nonce/join flow, direct links, magic link support"
```

---

## Self-Review

**Spec coverage check:**

| Spec requirement | Task |
|---|---|
| Browser knocks before agent creates peer | Task 5 (browser_ws), Task 4 (daemon) |
| Agent generates nonce per knock, stores with 60s TTL | Task 4 (`handleKnock`) |
| Atomic delete-and-verify for nonce | Task 4 (`handleJoin` — deletes under mutex before checking) |
| HMAC-SHA256(key=password, data=nonce) | Task 4 (Go), Task 7 (JS) |
| Password-less shares: skip verification | Task 4 (`handleJoin` checks `session.Password != ""`) |
| Signaling server routes blindly | Task 5 (browser_ws, agent_ws) |
| 3-strike limit: close browser WS | Task 5 (agent_ws), Task 3 (hub) |
| `auth_failed` re-prompts browser | Task 7 (browser JS `auth_failed` case + re-knock) |
| conn_id per browser connection | Task 5 (browser_ws generates) |
| Nonce sweep on each knock (no goroutine) | Task 4 (`handleKnock` sweeps inline) |
| `has_password` in nonce response | Task 4 (`handleKnock` sends it), Task 7 (JS reads it) |
| `password_required` removed from DataChannel hello | Task 1 |
| `password` removed from `list_request` | Task 1 + Task 7 |
| `authenticated` gate removed from Manager | Task 1 |
| `TestHandleFileRequest_UnauthenticatedBlocked` deleted | Task 1 |
| `/s/:code` route | Task 6 |
| Code auto-filled from URL path | Task 7 |
| Password read from URL hash, cleared immediately | Task 7 |
| `decodeURIComponent` for hash | Task 7 |
| Auto-submit when hash password present | Task 7 |
| Coordinated deploy (no backward compat) | No code needed — acknowledged in spec |
