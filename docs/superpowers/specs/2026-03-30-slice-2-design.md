# Slice 2 — File Transfer Design

**Date**: 2026-03-30
**Status**: Approved
**Reference**: [MVP Design](2026-03-30-mvp-design.md)

## Goal

Recipient clicks a file in the browser, it downloads end-to-end via the DataChannel.

## Decisions Summary

| Decision | Choice | Rationale |
|----------|--------|-----------|
| File scope | Auto-detect folder vs single file | Single code path handles both |
| List refresh | Static (once on connect) | Simple, matches GDrive/Dropbox behavior |
| Concurrent downloads | One-at-a-time | Simpler protocol, deferred parallel to future |
| Resume support | None | Fail on disconnect, deferred to future |
| Progress indication | Progress bar with % | Good UX, file size available from PROPFIND |
| Chunk size | 64KB | Balance: overhead vs backpressure granularity |
| Backpressure threshold | 256KB buffered | ~4 chunks in flight |

## Protocol Specification

Two frame types over the DataChannel:
- **Text frames**: JSON control messages (list_request, file_request, file_list, file_header, chunk_end, error)
- **Binary frames**: Raw file data chunks (ArrayBuffer in browser, []byte in Go)

### Message Types (Text Frames)

#### Browser → Agent

```json
// Request file listing (sent automatically when DataChannel opens)
{"type": "list_request"}

// Request specific file (only when no transfer in progress)
{"type": "file_request", "name": "document.pdf"}
```

#### Agent → Browser

```json
// File listing response
{
  "type": "file_list",
  "files": [
    {"name": "report.pdf", "size": 1048576, "mimeType": "application/pdf"},
    {"name": "photo.jpg", "size": 2097152, "mimeType": "image/jpeg"}
  ]
}

// File metadata before binary transfer begins
{
  "type": "file_header",
  "name": "report.pdf",
  "size": 1048576,
  "mimeType": "application/pdf"
}

// Transfer complete (sent after last binary chunk)
{"type": "chunk_end"}

// Error response
{"type": "error", "message": "file not found"}
```

### Binary Frames

File data is sent as raw binary DataChannel messages (not JSON). Each binary frame contains exactly one 64KB chunk (except the last which may be smaller).

### Message Sequence

```
Browser                          Agent
   |                                |
   |--- DataChannel opens --------->|
   |--- list_request -------------->|
   |                                |-- PROPFIND share URL
   |<-- file_list ------------------|   (folder: list contents)
   |                                |   (file: single item)
   |                                |
   [User clicks file]               |
   |--- file_request -------------->|
   |                                |-- GET file from WebDAV
   |<-- file_header ----------------|
   |<-- BINARY (64KB) --------------|
   |<-- BINARY (64KB) --------------|
   |<-- BINARY (remainder) -------->|
   |<-- chunk_end ------------------|
   |                                |
   [Browser assembles chunks, triggers download]
```

**Concurrent transfer behavior:** If a `file_request` arrives while a transfer is already in progress, the agent sends `error: "transfer in progress"` and ignores the request. The browser must wait for the current download to complete before requesting another file.

## Component Design

### 1. WebDAV Client (`agent/internal/opencloud/`)

```go
package opencloud

import "io"

// Client provides WebDAV access to OpenCloud public shares.
type Client struct {
    baseURL    string // e.g., https://host/remote.php/dav/public-files/{token}
    httpClient *http.Client
}

// FileInfo describes a file or folder entry.
type FileInfo struct {
    Name        string
    Size        int64
    ContentType string
}

// New creates a WebDAV client for the given public share URL.
// Returns error if URL is invalid or host not in allowed list.
func New(shareURL string, allowedHost string) (*Client, error)

// ListFiles returns file info for the share.
// For folder shares: returns all items.
// For file shares: returns single item.
func (c *Client) ListFiles() ([]FileInfo, error)

// GetFile streams the named file to the writer.
// Returns number of bytes written.
func (c *Client) GetFile(name string, w io.Writer) (int64, error)
```

**SSRF Protection:**
- Parse share URL, extract hostname
- Verify against `allowed_opencloud_host` config value
- Reject if mismatch

**Authentication:**
- Basic Auth with token as username, empty password
- `Authorization: Basic base64(token:)`

**PROPFIND Response Parsing:**
- Parse XML WebDAV response
- Extract `d:href`, `d:getcontentlength`, `d:getcontenttype`
- Skip directories (`d:collection`)

### 2. Peer Updates (`agent/internal/peer/`)

Add DataChannel message handling to existing Peer struct:

```go
// Peer additions
type Peer struct {
    // ... existing fields ...
    dc              *webrtc.DataChannel
    OnMessage       func(data []byte)  // Called for both text and binary frames
}

// Send transmits a message over the DataChannel.
func (p *Peer) Send(data []byte) error

// SetOnMessage sets the callback for incoming DataChannel messages.
func (p *Peer) SetOnMessage(handler func(data []byte))
```

### 3. File Transfer Manager (`agent/internal/transfer/`)

Manages active file transfers with backpressure:

```go
package transfer

import (
    "context"
    "io"
    "opencloudshare/agent/internal/opencloud"
)

// Manager handles file transfer state and backpressure.
type Manager struct {
    dc      DataChannel
    client  *opencloud.Client
}

// NewManager creates a transfer manager for the given DataChannel and WebDAV client.
func NewManager(dc DataChannel, client *opencloud.Client) *Manager

// HandleMessage processes incoming DataChannel messages.
func (m *Manager) HandleMessage(msg []byte)

// SendFile streams a file to the browser with backpressure handling.
func (m *Manager) SendFile(ctx context.Context, name string) error

type DataChannel interface {
    Send(data []byte) error
    BufferedAmount() uint64
}
```

**Backpressure Algorithm:**

```go
const (
    chunkSize     = 64 * 1024  // 64KB
    maxBuffer     = 256 * 1024 // 256KB
    sleepInterval = 10 * time.Millisecond
)

func (m *Manager) sendWithBackpressure(data []byte) error {
    for m.dc.BufferedAmount() > maxBuffer {
        time.Sleep(sleepInterval)
    }
    return m.dc.Send(data)
}
```

### 4. Browser UI Updates

**HTML Changes:**
- Hide code entry form after successful connection
- Show file list container
- Show download progress area

**JavaScript Message Handlers:**

```javascript
// Handle incoming DataChannel messages
dc.onmessage = (event) => {
  if (event.data instanceof ArrayBuffer) {
    // Binary frame: file data chunk
    appendChunk(new Uint8Array(event.data));
    return;
  }

  // Text frame: JSON control message
  const msg = JSON.parse(event.data);
  switch (msg.type) {
    case 'file_list': renderFileList(msg.files); break;
    case 'file_header': startDownload(msg); break;
    case 'chunk_end': completeDownload(); break;
    case 'error': showError(msg.message); break;
  }
};

dc.onopen = () => {
  // Automatically request file list when channel opens
  dc.send(JSON.stringify({type: 'list_request'}));
};

// Request and render file list
function renderFileList(files) {
  // Render clickable file list with sizes
  // Click handler sends file_request (only if not downloading)
}

// Download assembly
const chunks = [];
let currentFile = null;
let receivedBytes = 0;

function startDownload(header) {
  currentFile = header;
  chunks.length = 0;
  receivedBytes = 0;
  updateProgress(0, header.size);
}

function appendChunk(bytes) {
  chunks.push(bytes);
  receivedBytes += bytes.length;
  updateProgress(receivedBytes, currentFile.size);
}

function completeDownload() {
  // Combine all chunks into single Uint8Array
  const totalLength = chunks.reduce((sum, c) => sum + c.length, 0);
  const combined = new Uint8Array(totalLength);
  let offset = 0;
  for (const chunk of chunks) {
    combined.set(chunk, offset);
    offset += chunk.length;
  }

  const blob = new Blob([combined], { type: currentFile.mimeType });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = currentFile.name;
  a.click();
  URL.revokeObjectURL(url);
  resetDownloadState();
}
```

**Progress Bar:**

```html
<div id="progress-container" style="display:none">
  <div id="progress-bar" style="width:0%; background:green; height:20px;"></div>
  <span id="progress-text">0%</span>
</div>
```

## Error Handling

| Scenario | Agent Behavior | Browser Behavior |
|----------|---------------|------------------|
| PROPFIND fails | Log error, send `error: "share unavailable"` | Show error message, allow retry |
| File not found | Send `error: "file not found"` | Show error, return to file list |
| WebDAV timeout | Close chunk stream, send `error` | Show "transfer failed" |
| DataChannel closed mid-transfer | Stop reading, cleanup | Show "connection lost", reset UI |
| Invalid message | Send `error: "invalid request"` | Log error, ignore |
| Transfer in progress | Send `error: "transfer in progress"` | Ignore or disable other file clicks |

## Testing Strategy

**Unit Tests:**
- `opencloud/client_test.go`: PROPFIND XML parsing, URL validation, SSRF rejection
- `transfer/manager_test.go`: Backpressure calculation (mock DataChannel)

**Manual E2E Test:**
1. Start agent with folder share URL
2. Open browser, enter code
3. Verify file list displays
4. Click file, verify download completes
5. Verify progress bar updates
6. Test with single file share (auto-detect)
7. Test error case: invalid file request

## Future Enhancements

See [memory/future_parallel_downloads.md](../../../memory/future_parallel_downloads.md) for:
- Parallel concurrent downloads
- Resumable downloads (byte-range resume)
