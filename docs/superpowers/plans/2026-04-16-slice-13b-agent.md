# Slice 13b — Agent Transport Replacement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the agent's WebRTC PeerConnection/DataChannel transport with a go-libp2p stream transport that dials the 13a relay, so file transfer flows over a libp2p stream using the existing binary protocol.

**Architecture:** The agent becomes a go-libp2p `Host` with a stable Ed25519 identity persisted to disk. At startup it dials the relay multiaddr advertised in the signaling server's `welcome` message and registers a stream handler for protocol `/sharebridge/file/1.0.0`. When a browser completes the 13a knock/HMAC flow, the agent sends `auth_ok` to the signaling server (triggering JWT issuance), then waits for the browser's inbound libp2p stream via the relay circuit. The first frame on each stream carries `{share_code, conn_id}`; the agent looks up the session, builds a `DataChannel` adapter over the stream, and hands it to the existing unchanged `transfer.Manager`. The `internal/peer/` package and `github.com/pion/webrtc/v4` dependency are removed entirely.

**Tech Stack:** Go 1.26, `github.com/libp2p/go-libp2p`, libp2p transports: `websocket` (outbound relay connection), `webrtc` (direct path via DCUtR). Noise and yamux come by default.

**Prerequisites:** Slice 13a complete — relay is listening, welcome message carries `relay_multiaddr`, JWT issuance is gated on an `auth_ok` agent message (13a R2/R3).

---

## Scope Check

This plan covers only the agent binary (`agent/` module). The signaling server is unchanged from the 13a end state. The browser still speaks legacy WebRTC — we will not be able to do an end-to-end browser test until 13c ships. For end-to-end validation in this phase we use the go-libp2p test client that 13a introduced in `signaling-server/internal/relay/testdata_client_test.go`.

---

## File Structure

**New:**
- `agent/internal/identity/identity.go` — Ed25519 key persistence (load-or-generate pattern mirroring the relay's). One responsibility: give me a stable libp2p private key.
- `agent/internal/identity/identity_test.go`
- `agent/internal/transport/transport.go` — libp2p Host lifecycle: construct, dial relay, reserve circuit, reconnect on drop. Registers the file stream handler.
- `agent/internal/transport/stream_adapter.go` — wraps a `network.Stream` so it satisfies `transfer.DataChannel` (SendBinary, SendText, BufferedAmount, Close). Handles the framing that separates text from binary messages (libp2p streams are raw bytes — no builtin message kind like WebRTC DataChannel has).
- `agent/internal/transport/frame.go` — 5-byte length-prefixed framing: 1 byte kind (0x01=text, 0x02=binary) + 4-byte BE uint32 length + payload.
- `agent/internal/transport/transport_test.go`
- `agent/internal/transport/stream_adapter_test.go`
- `agent/internal/transport/frame_test.go`

**Modified:**
- `agent/internal/config/config.go` — add `RelayIdentityPath` field with default `~/.sharebridge/relay_identity.key`.
- `agent/internal/signaling/client.go` — remove `iceServers` state, remove `github.com/pion/webrtc/v4` import entirely, add `RelayMultiaddr` + `AgentPeerID` fields, expose `GetRelayMultiaddr()`. Welcome handler stores the multiaddr string. Also widen `Message` to carry `auth_ok`.
- `agent/internal/daemon/daemon.go` — drop `handleAnswer` and `handleICECandidate`; `createPeer` becomes `sendAuthOK`; add `Transport` field on `Daemon`; stream handler dispatches to a new `handleIncomingStream(connID, sessionCode, stream)` that constructs the `stream_adapter.StreamAdapter` + `transfer.Manager` and runs the transfer. Reads from the stream and routes bytes to `transfer.Manager.HandleMessage` (text frames) / ignores binary (agent is sender-only today). Remove `hasTURN`/`HasTURN()`. Remove pion imports.
- `agent/internal/daemon/daemon_test.go` — swap the `fakeSignaling` ICE stub for a `fakeTransport`; remove SDP/ICE test coverage; add stream-dispatch tests.
- `agent/cmd/agent/main.go` — construct identity + transport and inject into daemon.
- `agent/go.mod` / `agent/go.sum` — add `github.com/libp2p/go-libp2p` and `github.com/multiformats/go-multiaddr`; remove `github.com/pion/webrtc/v4`.

**Deleted:**
- `agent/internal/peer/peer.go`
- `agent/internal/peer/` (whole directory)

---

## Task 1: Add libp2p dependencies

**Files:**
- Modify: `agent/go.mod`
- Modify: `agent/go.sum` (via `go mod tidy`)

- [ ] **Step 1: Add libp2p deps, do not yet remove pion**

Run from the `agent/` module root:

```bash
go get github.com/libp2p/go-libp2p@latest github.com/multiformats/go-multiaddr@latest
```

- [ ] **Step 2: Tidy**

```bash
go mod tidy
go build ./...
```

Expected: both succeed. Pion remains in go.mod (we delete it in Task 9) so nothing regresses yet.

- [ ] **Step 3: Commit**

```bash
git add agent/go.mod agent/go.sum
git commit -m "chore(agent): add go-libp2p dependency for Slice 13b"
```

---

## Task 2: Identity key persistence

Mirrors the `signaling-server/internal/relay/identity.go` pattern from 13a.

**Files:**
- Create: `agent/internal/identity/identity.go`
- Create: `agent/internal/identity/identity_test.go`

- [ ] **Step 1: Write the failing test**

```go
package identity

import (
	"path/filepath"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
)

func TestLoadOrCreate_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent_identity.key")

	priv1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	if priv1 == nil {
		t.Fatal("expected non-nil private key")
	}

	priv2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}

	a, _ := crypto.MarshalPrivateKey(priv1)
	b, _ := crypto.MarshalPrivateKey(priv2)
	if string(a) != string(b) {
		t.Fatal("key changed across reloads — must be stable")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/identity/...`
Expected: FAIL with "undefined: identity.LoadOrCreate" (package has no implementation yet).

- [ ] **Step 3: Implement**

```go
package identity

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
)

// LoadOrCreate returns an Ed25519 libp2p private key persisted at path.
// The key is created on first call; subsequent calls return the same key.
// Directory is created with mode 0700; the file is written with mode 0600.
func LoadOrCreate(path string) (crypto.PrivKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		priv, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse identity key at %s: %w", path, err)
		}
		return priv, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read identity key at %s: %w", path, err)
	}

	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, fmt.Errorf("generate identity key: %w", err)
	}

	data, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal identity key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create identity dir: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return nil, fmt.Errorf("write identity key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("rename identity key: %w", err)
	}
	return priv, nil
}
```

- [ ] **Step 4: Run test, expect pass**

Run: `go test ./internal/identity/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/identity/
git commit -m "feat(agent): persistent libp2p identity key"
```

---

## Task 3: Frame codec

A libp2p stream is raw bytes — we need the same (text, binary) distinction the old WebRTC DataChannel gave us for free. Framing format: 1-byte kind (0x01 text, 0x02 binary) + 4-byte big-endian uint32 length + payload. Max payload 8 MiB (defensive cap; real chunks are 64 KiB).

**Files:**
- Create: `agent/internal/transport/frame.go`
- Create: `agent/internal/transport/frame_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package transport

import (
	"bytes"
	"io"
	"testing"
)

func TestFrame_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameText, []byte("hello")); err != nil {
		t.Fatalf("WriteFrame text: %v", err)
	}
	if err := WriteFrame(&buf, FrameBinary, []byte{0xde, 0xad, 0xbe, 0xef}); err != nil {
		t.Fatalf("WriteFrame binary: %v", err)
	}

	kind, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame 1: %v", err)
	}
	if kind != FrameText || string(payload) != "hello" {
		t.Fatalf("frame 1 mismatch: kind=%d payload=%q", kind, payload)
	}

	kind, payload, err = ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame 2: %v", err)
	}
	if kind != FrameBinary || !bytes.Equal(payload, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("frame 2 mismatch: kind=%d payload=%x", kind, payload)
	}
}

func TestFrame_RejectsOversized(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(FrameBinary))
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff}) // 4 GiB claimed length
	_, _, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected oversized frame to be rejected")
	}
}

func TestFrame_RejectsUnknownKind(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0x99)
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00})
	_, _, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected unknown kind to be rejected")
	}
}

func TestFrame_EOFBeforeHeader(t *testing.T) {
	var buf bytes.Buffer
	_, _, err := ReadFrame(&buf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF at stream end, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transport/...`
Expected: FAIL with "undefined: WriteFrame" etc.

- [ ] **Step 3: Implement**

```go
package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type FrameKind byte

const (
	FrameText   FrameKind = 0x01
	FrameBinary FrameKind = 0x02

	MaxFramePayload = 8 * 1024 * 1024 // 8 MiB defensive cap
)

// WriteFrame writes a single framed message to w.
func WriteFrame(w io.Writer, kind FrameKind, payload []byte) error {
	if kind != FrameText && kind != FrameBinary {
		return fmt.Errorf("unknown frame kind: 0x%x", byte(kind))
	}
	if len(payload) > MaxFramePayload {
		return fmt.Errorf("frame too large: %d bytes (max %d)", len(payload), MaxFramePayload)
	}
	var header [5]byte
	header[0] = byte(kind)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads a single framed message from r.
// Returns io.EOF cleanly when r is exhausted at a frame boundary.
func ReadFrame(r io.Reader) (FrameKind, []byte, error) {
	var header [5]byte
	n, err := io.ReadFull(r, header[:])
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return 0, nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, fmt.Errorf("truncated frame header: %w", err)
		}
		return 0, nil, err
	}
	kind := FrameKind(header[0])
	if kind != FrameText && kind != FrameBinary {
		return 0, nil, fmt.Errorf("unknown frame kind: 0x%x", header[0])
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > MaxFramePayload {
		return 0, nil, fmt.Errorf("frame too large: %d bytes (max %d)", length, MaxFramePayload)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("read frame payload: %w", err)
	}
	return kind, payload, nil
}
```

- [ ] **Step 4: Run test, expect pass**

Run: `go test ./internal/transport/...`
Expected: PASS all four subtests.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/transport/frame.go agent/internal/transport/frame_test.go
git commit -m "feat(agent): text/binary framing for libp2p streams"
```

---

## Task 4: Stream adapter satisfying transfer.DataChannel

The transfer package's `DataChannel` interface from [`agent/internal/transfer/manager.go:20-26`](agent/internal/transfer/manager.go) is:

```go
type DataChannel interface {
    SendBinary(data []byte) error
    SendText(text string) error
    BufferedAmount() uint64
    Close() error
}
```

libp2p streams don't expose buffered-amount; yamux applies credit-based flow control internally so `Write` blocks naturally once the peer's window is full. We return 0 from `BufferedAmount()` — the `sendWithBackpressure` loop in `transfer/manager.go:261-266` simply won't sleep. That is correct for libp2p because `Write` already provides backpressure.

**Files:**
- Create: `agent/internal/transport/stream_adapter.go`
- Create: `agent/internal/transport/stream_adapter_test.go`

- [ ] **Step 1: Write the failing test**

```go
package transport

import (
	"bytes"
	"io"
	"testing"
)

// readWriteCloser is a minimal stand-in for a network.Stream.
type readWriteCloser struct {
	r      io.Reader
	w      *bytes.Buffer
	closed bool
}

func (rw *readWriteCloser) Read(p []byte) (int, error)  { return rw.r.Read(p) }
func (rw *readWriteCloser) Write(p []byte) (int, error) { return rw.w.Write(p) }
func (rw *readWriteCloser) Close() error                { rw.closed = true; return nil }

func TestStreamAdapter_SendText(t *testing.T) {
	rw := &readWriteCloser{r: bytes.NewReader(nil), w: &bytes.Buffer{}}
	a := NewStreamAdapter(rw)

	if err := a.SendText(`{"type":"hello"}`); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	kind, payload, err := ReadFrame(rw.w)
	if err != nil {
		t.Fatalf("decode written frame: %v", err)
	}
	if kind != FrameText || string(payload) != `{"type":"hello"}` {
		t.Fatalf("wrong frame written: kind=%d payload=%q", kind, payload)
	}
}

func TestStreamAdapter_SendBinary(t *testing.T) {
	rw := &readWriteCloser{r: bytes.NewReader(nil), w: &bytes.Buffer{}}
	a := NewStreamAdapter(rw)

	if err := a.SendBinary([]byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("SendBinary: %v", err)
	}

	kind, payload, err := ReadFrame(rw.w)
	if err != nil {
		t.Fatalf("decode written frame: %v", err)
	}
	if kind != FrameBinary || !bytes.Equal(payload, []byte{0x01, 0x02, 0x03}) {
		t.Fatalf("wrong frame written: kind=%d payload=%x", kind, payload)
	}
}

func TestStreamAdapter_BufferedAmountAlwaysZero(t *testing.T) {
	a := NewStreamAdapter(&readWriteCloser{r: bytes.NewReader(nil), w: &bytes.Buffer{}})
	if got := a.BufferedAmount(); got != 0 {
		t.Fatalf("BufferedAmount = %d, want 0", got)
	}
}

func TestStreamAdapter_Close(t *testing.T) {
	rw := &readWriteCloser{r: bytes.NewReader(nil), w: &bytes.Buffer{}}
	a := NewStreamAdapter(rw)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rw.closed {
		t.Fatal("underlying stream not closed")
	}
}
```

- [ ] **Step 2: Run test, verify it fails**

Run: `go test ./internal/transport/...`
Expected: FAIL with "undefined: NewStreamAdapter".

- [ ] **Step 3: Implement**

```go
package transport

import (
	"io"
	"sync"
)

// StreamAdapter wraps an io.ReadWriteCloser (typically a libp2p network.Stream)
// so it satisfies the transfer.DataChannel interface expected by
// transfer.Manager.
//
// Framing: every SendText/SendBinary call writes one length-prefixed frame,
// so the peer can distinguish text from binary messages without a separate
// signaling layer.
//
// BufferedAmount always returns 0: yamux (the libp2p stream muxer) enforces
// backpressure internally by blocking Write once the peer's receive window is
// full, so the manager's sendWithBackpressure loop does not need to sleep.
type StreamAdapter struct {
	rw     io.ReadWriteCloser
	writeM sync.Mutex // serialises Write calls so frames never interleave
}

// NewStreamAdapter wraps rw.
func NewStreamAdapter(rw io.ReadWriteCloser) *StreamAdapter {
	return &StreamAdapter{rw: rw}
}

// SendText sends a JSON control message as a FrameText frame.
func (s *StreamAdapter) SendText(text string) error {
	s.writeM.Lock()
	defer s.writeM.Unlock()
	return WriteFrame(s.rw, FrameText, []byte(text))
}

// SendBinary sends a binary chunk as a FrameBinary frame.
func (s *StreamAdapter) SendBinary(data []byte) error {
	s.writeM.Lock()
	defer s.writeM.Unlock()
	return WriteFrame(s.rw, FrameBinary, data)
}

// BufferedAmount is always 0. See type doc.
func (s *StreamAdapter) BufferedAmount() uint64 { return 0 }

// Close closes the underlying stream.
func (s *StreamAdapter) Close() error { return s.rw.Close() }
```

- [ ] **Step 4: Run test, expect pass**

Run: `go test ./internal/transport/...`
Expected: PASS all four subtests.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/transport/stream_adapter.go agent/internal/transport/stream_adapter_test.go
git commit -m "feat(agent): libp2p stream adapter satisfying transfer.DataChannel"
```

---

## Task 5: Transport Host — construction, relay dial, reconnect

The `Transport` struct owns the libp2p `Host`, the outbound connection to the relay, and the file-stream handler registration. It exposes a callback-style API: `OnStream(handler)` lets the daemon plug in its dispatcher.

**Files:**
- Create: `agent/internal/transport/transport.go`
- Create: `agent/internal/transport/transport_test.go`
- Modify: `agent/internal/config/config.go` (add `RelayIdentityPath`)

- [ ] **Step 1: Add RelayIdentityPath to Config**

Edit [`agent/internal/config/config.go`](agent/internal/config/config.go): inside the `Config` struct add:

```go
RelayIdentityPath string `json:"relay_identity_path,omitempty"` // defaults to ~/.sharebridge/agent_identity.key
```

And in `load()` after the existing env overrides, add a fallback to the default:

```go
if cfg.RelayIdentityPath == "" {
    homeDir, err := os.UserHomeDir()
    if err == nil {
        cfg.RelayIdentityPath = filepath.Join(homeDir, ".sharebridge", "agent_identity.key")
    }
}
```

Build: `go build ./...` — expect success.

- [ ] **Step 2: Write the failing construction test**

```go
package transport

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
)

func newTestPrivKey(t *testing.T) crypto.PrivKey {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv
}

func TestTransport_NewStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tr, err := New(ctx, Options{PrivKey: newTestPrivKey(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tr.PeerID() == "" {
		t.Fatal("PeerID empty")
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestTransport_StreamHandlerReceivesBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server, err := New(ctx, Options{PrivKey: newTestPrivKey(t)})
	if err != nil {
		t.Fatalf("server New: %v", err)
	}
	defer server.Close()

	received := make(chan []byte, 1)
	server.OnStream(func(s StreamInfo) {
		defer s.Stream.Close()
		buf := make([]byte, 16)
		n, _ := s.Stream.Read(buf)
		received <- buf[:n]
	})

	// Dial server directly from a second host (no relay in this unit test).
	client, err := New(ctx, Options{PrivKey: newTestPrivKey(t)})
	if err != nil {
		t.Fatalf("client New: %v", err)
	}
	defer client.Close()

	if err := client.ConnectDirect(ctx, server.Host().Peerstore().PeerInfo(server.Host().ID()).Addrs, server.Host().ID()); err != nil {
		t.Fatalf("connect direct: %v", err)
	}
	stream, err := client.Host().NewStream(ctx, server.Host().ID(), FileProtocolID)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	if _, err := stream.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	stream.CloseWrite()

	select {
	case got := <-received:
		if string(got) != "hi" {
			t.Fatalf("got %q want %q", got, "hi")
		}
	case <-ctx.Done():
		t.Fatal("stream handler never fired")
	}
}
```

Add this helper import path at top of test file if not present.

- [ ] **Step 3: Run test, expect it to fail**

Run: `go test ./internal/transport/ -run TestTransport`
Expected: FAIL with "undefined: transport.New" / "undefined: FileProtocolID" / "undefined: StreamInfo".

- [ ] **Step 4: Implement Transport**

```go
package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multiaddr"
)

// FileProtocolID is the libp2p protocol for ShareBridge file-transfer streams.
const FileProtocolID protocol.ID = "/sharebridge/file/1.0.0"

// StreamInfo is what the daemon's stream handler receives.
type StreamInfo struct {
	Stream network.Stream
	Peer   peer.ID
}

// StreamHandler is the daemon's callback for each inbound file stream.
type StreamHandler func(StreamInfo)

// Options configures Transport.
type Options struct {
	PrivKey crypto.PrivKey // libp2p identity; required
}

// Transport owns the libp2p Host and the outbound relay connection.
type Transport struct {
	host host.Host

	mu       sync.Mutex
	onStream StreamHandler
}

// New creates a libp2p Host with the given identity. It does not dial the
// relay — call DialRelay afterwards. The Host listens on no addresses because
// the agent is outbound-only.
func New(ctx context.Context, opts Options) (*Transport, error) {
	if opts.PrivKey == nil {
		return nil, errors.New("transport: PrivKey is required")
	}

	h, err := libp2p.New(
		libp2p.Identity(opts.PrivKey),
		libp2p.NoListenAddrs, // agent does not accept inbound direct dials
	)
	if err != nil {
		return nil, fmt.Errorf("libp2p.New: %w", err)
	}

	t := &Transport{host: h}
	h.SetStreamHandler(FileProtocolID, t.handleStream)
	return t, nil
}

// Host exposes the underlying libp2p Host (used in tests).
func (t *Transport) Host() host.Host { return t.host }

// PeerID returns the libp2p peer ID string.
func (t *Transport) PeerID() string { return t.host.ID().String() }

// OnStream sets the callback for each inbound file stream. Must be called
// before any stream can arrive; typically wired during daemon init.
func (t *Transport) OnStream(h StreamHandler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onStream = h
}

func (t *Transport) handleStream(s network.Stream) {
	t.mu.Lock()
	h := t.onStream
	t.mu.Unlock()
	if h == nil {
		s.Reset()
		return
	}
	h(StreamInfo{Stream: s, Peer: s.Conn().RemotePeer()})
}

// DialRelay dials the relay multiaddr and keeps a persistent connection open.
// relayMultiaddr must include the relay's /p2p/<peer-id> component.
func (t *Transport) DialRelay(ctx context.Context, relayMultiaddr string) error {
	addr, err := multiaddr.NewMultiaddr(relayMultiaddr)
	if err != nil {
		return fmt.Errorf("parse relay multiaddr %q: %w", relayMultiaddr, err)
	}
	info, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return fmt.Errorf("extract peer info: %w", err)
	}
	if err := t.host.Connect(ctx, *info); err != nil {
		return fmt.Errorf("connect to relay: %w", err)
	}
	return nil
}

// ConnectDirect dials a peer by its addresses. Test-only helper used in
// transport_test.go — production code always goes via DialRelay.
func (t *Transport) ConnectDirect(ctx context.Context, addrs []multiaddr.Multiaddr, id peer.ID) error {
	return t.host.Connect(ctx, peer.AddrInfo{ID: id, Addrs: addrs})
}

// Close shuts down the Host.
func (t *Transport) Close() error { return t.host.Close() }
```

- [ ] **Step 5: Run tests, expect pass**

Run: `go test ./internal/transport/...`
Expected: PASS. The two Transport tests plus all earlier frame + adapter tests.

- [ ] **Step 6: Add the relay-dial integration test**

Append to `transport_test.go`:

```go
func TestTransport_DialsRelayAndAcceptsCircuitStream(t *testing.T) {
	// This test exercises DialRelay against a real libp2p host acting as
	// a plain listener (no circuit) — it validates the dial path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Build a listener host (stands in for the relay — no relay protocol needed
	// here; we only verify that DialRelay establishes a connection).
	listenerHost, err := libp2p.New(
		libp2p.Identity(newTestPrivKey(t)),
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
	)
	if err != nil {
		t.Fatalf("listener host: %v", err)
	}
	defer listenerHost.Close()

	tr, err := New(ctx, Options{PrivKey: newTestPrivKey(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tr.Close()

	// Build a /p2p/... multiaddr for the listener.
	var ma string
	for _, a := range listenerHost.Addrs() {
		ma = a.String() + "/p2p/" + listenerHost.ID().String()
		break
	}
	if ma == "" {
		t.Fatal("listener has no addrs")
	}

	if err := tr.DialRelay(ctx, ma); err != nil {
		t.Fatalf("DialRelay: %v", err)
	}
	if len(tr.Host().Network().ConnsToPeer(listenerHost.ID())) == 0 {
		t.Fatal("no connection to listener after DialRelay")
	}
}
```

Add the `"github.com/libp2p/go-libp2p"` import to the test file. Run `go test ./internal/transport/...` — expect PASS.

- [ ] **Step 7: Commit**

```bash
git add agent/internal/config/config.go agent/internal/transport/transport.go agent/internal/transport/transport_test.go
git commit -m "feat(agent): libp2p Host transport with relay dial + stream handler"
```

---

## Task 6: Signaling client — drop ICE, accept relay multiaddr

**Files:**
- Modify: `agent/internal/signaling/client.go`

- [ ] **Step 1: Strip the pion import, add RelayMultiaddr + AuthOK fields**

Replace the top of [`agent/internal/signaling/client.go`](agent/internal/signaling/client.go) (through the Message/Client struct definitions) with:

```go
package signaling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Message is any message received from the signaling server.
type Message struct {
	Type           string          `json:"type"`
	SessionID      string          `json:"session_id,omitempty"` // Share code
	PeerID         string          `json:"peer_id,omitempty"`    // Legacy field (kept for hub compatibility)
	Err            string          `json:"message,omitempty"`
	Code           string          `json:"code,omitempty"`
	ConnID         string          `json:"conn_id,omitempty"`
	HMAC           string          `json:"hmac,omitempty"`
	Reconnected    bool            `json:"reconnected,omitempty"`
	RelayMultiaddr string          `json:"relay_multiaddr,omitempty"`
	BrowserPeerID  string          `json:"browser_peer_id,omitempty"`
	Candidate      json.RawMessage `json:"-"` // unused after Slice 13b; kept off-wire for compile compat
	SDP            string          `json:"-"` // unused after Slice 13b
}

// Client manages a WebSocket connection to the signaling server.
type Client struct {
	serverURL string
	apiKey    string
	agentID   string
	conn      *websocket.Conn
	OnMessage func(msg Message)
	mu        sync.Mutex

	relayMultiaddr string

	pendingReg   chan Message
	pendingRegMu sync.Mutex
}
```

Remove the old `ICEServer` struct entirely, remove the `iceServers` field, and remove the `"github.com/pion/webrtc/v4"` import.

- [ ] **Step 2: Replace GetICEServers with GetRelayMultiaddr**

Find the existing `GetICEServers()` method and replace it with:

```go
// GetRelayMultiaddr returns the relay's libp2p multiaddr received in welcome.
func (c *Client) GetRelayMultiaddr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.relayMultiaddr
}
```

- [ ] **Step 3: Update the welcome branch in Listen**

Replace the `if msg.Type == "welcome" && len(msg.ICEServers) > 0 { ... }` block with:

```go
if msg.Type == "welcome" && msg.RelayMultiaddr != "" {
    c.mu.Lock()
    c.relayMultiaddr = msg.RelayMultiaddr
    c.mu.Unlock()
}
```

- [ ] **Step 4: Build and run package tests**

Run: `go build ./internal/signaling/... && go test ./internal/signaling/...`
Expected: build succeeds. If existing tests reference `ICEServers` or `GetICEServers`, update them to `RelayMultiaddr` / `GetRelayMultiaddr` in the same edit. If the tests also build a fake `webrtc.ICEServer`, delete those stubs.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/signaling/
git commit -m "refactor(agent): signaling client accepts relay_multiaddr, drops ICE"
```

---

## Task 7: Daemon — dispatch streams, send auth_ok, drop WebRTC handlers

This is the biggest edit. The daemon stops owning `peer.Peer` instances and instead owns a `Transport` handle plus a `connID → *Session` lookup map so the stream handler can find the right session from the first frame.

**Files:**
- Modify: `agent/internal/daemon/daemon.go`

- [ ] **Step 1: Update imports and Daemon fields**

In [`agent/internal/daemon/daemon.go`](agent/internal/daemon/daemon.go):

- Remove imports: `"github.com/pion/webrtc/v4"`, `"sharebridge/agent/internal/peer"`.
- Add import: `"sharebridge/agent/internal/transport"`.

Replace the `Session` struct's `peers` field:

```go
// Was: peers map[string]*peer.Peer
// New: streams map[string]*transport.StreamAdapter // connID → adapter
streams map[string]*transport.StreamAdapter
```

Replace the `SignalingClientInterface`:

```go
// SignalingClientInterface defines the interface for signaling client.
type SignalingClientInterface interface {
    Connect(ctx context.Context) error
    RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool) (string, bool, error)
    DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error
    Send(ctx context.Context, msg any) error
    GetRelayMultiaddr() string
    Listen(ctx context.Context) error
    SetOnMessage(handler func(signaling.Message))
}
```

Add a `TransportInterface`:

```go
// TransportInterface defines the subset of transport.Transport the daemon uses.
type TransportInterface interface {
    DialRelay(ctx context.Context, relayMultiaddr string) error
    OnStream(h transport.StreamHandler)
    PeerID() string
    Close() error
}
```

Extend the `Daemon` struct: add `transport TransportInterface` field; remove `hasTURN bool`. Remove `HasTURN()` method.

- [ ] **Step 2: Update constructors**

Modify `New`:

```go
func New(cfgMgr ConfigManagerInterface, st StoreInterface, tr TransportInterface) (*Daemon, error) {
    cfg := cfgMgr.Get()
    agentID := st.GetAgentID()
    sig := signaling.New(cfg.SignalingURL, cfg.APIKey, agentID)
    d := &Daemon{
        config:    cfg,
        configMgr: cfgMgr,
        store:     st,
        signaling: sig,
        transport: tr,
        sessions:  make(map[string]*Session),
        nonces:    make(map[string]nonceEntry),
        startTime: time.Now(),
    }
    tr.OnStream(d.handleIncomingStream)
    return d, nil
}
```

And `NewWithSignaling` similarly — add `tr TransportInterface` parameter, wire it and call `tr.OnStream(d.handleIncomingStream)`.

- [ ] **Step 3: Replace the welcome handler**

In `handleSignalingMessage`, the `case "welcome":` body becomes:

```go
case "welcome":
    log.Println("agent authenticated with signaling server")
    d.signalingConnected = true
    relayMA := d.signaling.GetRelayMultiaddr()
    if relayMA == "" {
        log.Printf("welcome missing relay_multiaddr — transport will be unreachable")
        return
    }
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := d.transport.DialRelay(ctx, relayMA); err != nil {
            log.Printf("dial relay %s: %v", relayMA, err)
        } else {
            log.Printf("connected to relay %s", relayMA)
        }
    }()
```

Delete the `case "answer":` and `case "ice_candidate":` branches entirely.

- [ ] **Step 4: Replace createPeer with sendAuthOK**

Delete `createPeer`, `handleAnswer`, `handleICECandidate`, `hasTURNServer`. Update `handleJoin` — at the end where HMAC verification succeeded, the old code said `go d.createPeer(connID, sessionCode)`; replace that call with:

```go
go d.sendAuthOK(connID, sessionCode)
```

Then add `sendAuthOK`:

```go
// sendAuthOK notifies the signaling server that a browser cleared the HMAC
// pre-challenge. The server uses this as the trigger to issue a JWT and
// return relay_multiaddr + token to the browser. The browser then dials the
// relay, which validates the JWT and opens a circuit to this agent; the
// stream arrives via handleIncomingStream.
func (d *Daemon) sendAuthOK(connID, sessionCode string) {
    d.mu.RLock()
    _, ok := d.sessions[sessionCode]
    d.mu.RUnlock()
    if !ok {
        log.Printf("sendAuthOK for unknown session %s", sessionCode)
        return
    }
    if err := d.signaling.Send(context.Background(), map[string]any{
        "type":    "auth_ok",
        "conn_id": connID,
        "code":    sessionCode,
    }); err != nil {
        log.Printf("send auth_ok for session %s conn %s: %v", sessionCode, connID, err)
    }
}
```

**Important (13a R2 follow-through):** For **no-password shares** the current agent in `handleJoin` skips HMAC verification and currently proceeds straight to `createPeer`. After this change it will call `sendAuthOK`. That matches the architecture: every successful join — password or not — funnels through `auth_ok`.

- [ ] **Step 5: Add handleIncomingStream**

Below `sendAuthOK`, add:

```go
// handleIncomingStream is invoked by Transport for each inbound file stream.
// Framing: the first frame on the stream is a FrameText JSON envelope
//   {"type":"open","share_code":"<code>","conn_id":"<id>"}
// The agent looks up the session, attaches a transfer.Manager, and serves.
func (d *Daemon) handleIncomingStream(info transport.StreamInfo) {
    stream := info.Stream
    kind, payload, err := transport.ReadFrame(stream)
    if err != nil {
        log.Printf("read open frame from %s: %v", info.Peer, err)
        stream.Reset()
        return
    }
    if kind != transport.FrameText {
        log.Printf("first frame from %s is binary; expected text open envelope", info.Peer)
        stream.Reset()
        return
    }
    var env struct {
        Type      string `json:"type"`
        ShareCode string `json:"share_code"`
        ConnID    string `json:"conn_id"`
    }
    if err := json.Unmarshal(payload, &env); err != nil || env.Type != "open" {
        log.Printf("bad open envelope from %s: %v (raw=%s)", info.Peer, err, string(payload))
        stream.Reset()
        return
    }

    d.mu.RLock()
    session, ok := d.sessions[env.ShareCode]
    d.mu.RUnlock()
    if !ok {
        log.Printf("open for unknown session %s from %s", env.ShareCode, info.Peer)
        stream.Reset()
        return
    }

    adapter := transport.NewStreamAdapter(stream)
    session.mu.Lock()
    session.streams[env.ConnID] = adapter
    session.mu.Unlock()

    tm := transfer.NewManager(adapter, session.webdavClient, session.MaxDownloads)
    tm.SetDownloadCount(session.Downloads)
    tm.OnSessionExpired = func() {
        d.signaling.Send(context.Background(), map[string]any{
            "type":       "session_expired",
            "session_id": env.ShareCode,
            "peer_id":    env.ConnID,
        })
    }
    tm.OnDownloadComplete = func(bytesTransferred int64) {
        session.mu.Lock()
        session.Downloads++
        newCount := session.Downloads
        session.mu.Unlock()
        if _, err := d.store.IncrementDownloads(env.ShareCode); err != nil {
            log.Printf("warning: could not persist download count: %v", err)
        }
        d.signaling.DownloadComplete(context.Background(), env.ShareCode, bytesTransferred)
        log.Printf("download complete for session %s (count: %d)", env.ShareCode, newCount)
    }

    log.Printf("file stream open: session=%s conn=%s peer=%s", env.ShareCode, env.ConnID, info.Peer)
    tm.HandleOpen()

    // Read loop: decode frames and route text frames to the manager.
    go func() {
        defer func() {
            session.mu.Lock()
            delete(session.streams, env.ConnID)
            session.mu.Unlock()
            stream.Close()
        }()
        for {
            kind, payload, err := transport.ReadFrame(stream)
            if err != nil {
                return
            }
            if kind == transport.FrameText {
                tm.HandleMessage(payload)
            }
            // Binary frames from browser are unexpected today; ignored.
        }
    }()
}
```

- [ ] **Step 6: Update Stop() to close streams**

Replace the peer-closing block inside `Stop()`:

```go
for _, session := range d.sessions {
    session.mu.Lock()
    for connID, adapter := range session.streams {
        if err := adapter.Close(); err != nil {
            log.Printf("close stream %s: %v", connID, err)
        }
    }
    session.streams = make(map[string]*transport.StreamAdapter)
    session.mu.Unlock()
}
if d.transport != nil {
    if err := d.transport.Close(); err != nil {
        log.Printf("close transport: %v", err)
    }
}
```

Do the same substitution inside `RevokeSession` and `pruneExpiredSessions` where `session.peers` is iterated.

- [ ] **Step 7: Initialise streams map in CreateSession and loadSessionsFromStore**

Anywhere a `&Session{ ... peers: make(map[string]*peer.Peer) ... }` literal exists, replace with `streams: make(map[string]*transport.StreamAdapter)`.

- [ ] **Step 8: Build**

Run: `go build ./...` from `agent/`.
Expected: build passes (tests may still reference old methods — that's Task 8).

- [ ] **Step 9: Commit**

```bash
git add agent/internal/daemon/daemon.go
git commit -m "feat(agent): stream-based transfer via libp2p transport"
```

---

## Task 8: Update daemon tests

**Files:**
- Modify: `agent/internal/daemon/daemon_test.go`

- [ ] **Step 1: Inspect current tests**

Run: `go test ./internal/daemon/...`
Expected: FAIL — tests reference removed symbols (`handleAnswer`, `ICEServer`, `createPeer`, `hasTURNServer`, etc.).

- [ ] **Step 2: Replace the fake signaling client**

Find `fakeSignaling` in `daemon_test.go`. Update its methods:

- Delete `GetICEServers()`.
- Add `GetRelayMultiaddr() string { return f.relayMultiaddr }` and a corresponding field.

Add a `fakeTransport` in the same file:

```go
type fakeTransport struct {
    onStream transport.StreamHandler
    dialed   string
    closed   bool
}

func (f *fakeTransport) DialRelay(_ context.Context, ma string) error { f.dialed = ma; return nil }
func (f *fakeTransport) OnStream(h transport.StreamHandler)           { f.onStream = h }
func (f *fakeTransport) PeerID() string                               { return "12D3KooTest" }
func (f *fakeTransport) Close() error                                 { f.closed = true; return nil }
```

- [ ] **Step 3: Rewrite HMAC-success test**

Locate the test that asserts `handleJoin` produced a peer (previously `len(session.peers) == 1`). Replace with:

```go
func TestHandleJoin_HMACSuccessSendsAuthOK(t *testing.T) {
    sig := &fakeSignaling{}
    tr := &fakeTransport{}
    d := newTestDaemon(t, sig, tr) // update this helper as needed
    code := seedSession(t, d, "testpassword")
    nonce := seedNonce(t, d, "conn-1")
    hmac := computeHMAC(t, "testpassword", nonce)

    d.handleJoin("conn-1", code, hmac)

    // wait briefly for the goroutine to enqueue the send
    waitFor(t, func() bool { return sig.sentType("auth_ok") })

    if got := sig.lastSent(); got["type"] != "auth_ok" || got["conn_id"] != "conn-1" || got["code"] != code {
        t.Fatalf("auth_ok not sent correctly, got %+v", got)
    }
}
```

Helpers (`newTestDaemon`, `seedSession`, `seedNonce`, `computeHMAC`, `waitFor`, `sentType`, `lastSent`) likely already exist in the existing test file — wire them up to the new shape, or add thin versions if they don't.

- [ ] **Step 4: Add stream-dispatch test**

```go
func TestHandleIncomingStream_RoutesToTransferManager(t *testing.T) {
    sig := &fakeSignaling{}
    tr := &fakeTransport{}
    d := newTestDaemon(t, sig, tr)
    code := seedSession(t, d, "")

    // Build a fake network.Stream using an in-memory pipe so ReadFrame works.
    clientEnd, serverEnd := net.Pipe()
    go func() {
        _ = transport.WriteFrame(clientEnd, transport.FrameText,
            []byte(fmt.Sprintf(`{"type":"open","share_code":%q,"conn_id":"c-1"}`, code)))
    }()

    tr.onStream(transport.StreamInfo{
        Stream: &pipeStream{ReadWriteCloser: serverEnd}, // adapter satisfying network.Stream
        Peer:   "12D3KooTest",
    })

    // Because HandleOpen sends a hello text frame, we can decode it off the client end
    // to prove the transfer.Manager was wired in.
    kind, payload, err := transport.ReadFrame(clientEnd)
    if err != nil {
        t.Fatalf("read hello: %v", err)
    }
    if kind != transport.FrameText || !bytes.Contains(payload, []byte(`"hello"`)) {
        t.Fatalf("expected hello frame, got kind=%d payload=%s", kind, payload)
    }
}
```

The `pipeStream` helper is a small wrapper whose only job is to satisfy the `network.Stream` interface subset that `StreamAdapter` touches (`Read`, `Write`, `Close`). Because `StreamAdapter` takes `io.ReadWriteCloser`, a `net.Conn` already works — the `pipeStream` wrapper is only necessary if you want to type-assert to `network.Stream`. Simplest: change `handleIncomingStream` to call `transport.NewStreamAdapter(stream)` directly (which we already do — it takes `io.ReadWriteCloser`). Done.

Remove from the test file: every import of `github.com/pion/webrtc/v4`, every reference to `fakeSignaling.iceServers`, `hasTURN`, `HasTURN`, `handleAnswer`, `handleICECandidate`.

- [ ] **Step 5: Build and run**

Run: `go test ./internal/daemon/...`
Expected: PASS. Iterate on compilation errors until everything green. Do not invent tests for deleted behaviour.

- [ ] **Step 6: Commit**

```bash
git add agent/internal/daemon/daemon_test.go
git commit -m "test(agent): update daemon tests for libp2p transport"
```

---

## Task 9: Delete peer package, drop pion dependency

**Files:**
- Delete: `agent/internal/peer/peer.go`
- Modify: `agent/go.mod`, `agent/go.sum`

- [ ] **Step 1: Verify no remaining imports**

Run:

```bash
rg -l "sharebridge/agent/internal/peer" agent/
rg -l "github.com/pion/webrtc/v4" agent/
```

Expected: both produce no output. If any results remain, fix those files before deleting.

- [ ] **Step 2: Delete the package**

```bash
rm -rf agent/internal/peer
```

- [ ] **Step 3: Remove pion from go.mod**

```bash
cd agent && go mod tidy
```

Expected: `github.com/pion/webrtc/v4` and all `github.com/pion/*` indirect dependencies disappear from go.mod / go.sum.

- [ ] **Step 4: Build + test everything**

```bash
cd agent && go build ./... && go test ./...
```

Expected: PASS across the module.

- [ ] **Step 5: Commit**

```bash
git add agent/internal/peer agent/go.mod agent/go.sum
git commit -m "refactor(agent): remove pion/webrtc and internal/peer"
```

---

## Task 10: Wire transport into main and run end-to-end against 13a relay

**Files:**
- Modify: `agent/cmd/agent/main.go`

- [ ] **Step 1: Read current main.go to find the daemon construction site**

Run: `rg -n "daemon.New" agent/cmd/agent/main.go`

- [ ] **Step 2: Construct identity and transport**

Before the `daemon.New(...)` call, add:

```go
priv, err := identity.LoadOrCreate(cfgMgr.Get().RelayIdentityPath)
if err != nil {
    log.Fatalf("load identity: %v", err)
}
tr, err := transport.New(ctx, transport.Options{PrivKey: priv})
if err != nil {
    log.Fatalf("create transport: %v", err)
}
defer tr.Close()
```

Update the `daemon.New(cfgMgr, st)` call to `daemon.New(cfgMgr, st, tr)`. Add imports:

```go
"sharebridge/agent/internal/identity"
"sharebridge/agent/internal/transport"
```

Remove any remaining pion-related code paths (there should be none after Tasks 7–9).

- [ ] **Step 3: Build**

```bash
cd agent && go build ./...
```

Expected: success.

- [ ] **Step 4: End-to-end smoke test against 13a relay**

You need both a running 13a signaling server and a test browser. Since the browser still speaks WebRTC (13c), use the go-libp2p test client from 13a. In a terminal:

```bash
# Terminal 1: run signaling server with relay embedded (13a end state)
cd signaling-server && go run ./cmd/server

# Terminal 2: run agent
cd agent && SIGNALING_SERVER=ws://127.0.0.1:8080 \
    SHAREBRIDGE_API_KEY=<test key from pb admin> \
    go run ./cmd/agent

# Terminal 3: run the go-libp2p test client
cd signaling-server && go test ./internal/relay/ -run TestEndToEnd_BrowserDownload -v
```

Expected: the test client registers a share from the agent (indirectly by pre-seeding PocketBase), obtains a JWT, dials the relay, opens a `/sharebridge/file/1.0.0` stream, sends the open envelope, receives a `hello` text frame, and successfully issues `list_request` + `file_request` against a small test file.

If this test doesn't exist in 13a, stop and extend 13a's relay test harness first. Do not fake the end-to-end test.

- [ ] **Step 5: Commit**

```bash
git add agent/cmd/agent/main.go
git commit -m "feat(agent): wire libp2p transport + identity in main"
```

---

## Task 11: Manual verification checklist + PR

- [ ] **Step 1: Manual checks**

- [ ] Start agent; observe "connected to relay /dns4/..." log line.
- [ ] Agent identity key appears at `~/.sharebridge/agent_identity.key` with mode 0600.
- [ ] Kill and restart agent; peer ID in logs is unchanged.
- [ ] `grep -R "pion" agent/` returns nothing except possibly go.sum if tidy missed it (then re-run tidy).
- [ ] `go vet ./...` clean.
- [ ] `go test ./...` clean.

- [ ] **Step 2: Push branch and open PR**

```bash
git push -u origin slice-13b-agent
```

PR description must flag these known limitations:

> - End-to-end browser download is **not** yet possible: 13c still ships the legacy WebRTC browser. E2E coverage in this PR is via the go-libp2p test client in `signaling-server/internal/relay/`.
> - DCUtR direct-path upgrade is not yet exercised — the agent Host is configured for outbound-only and doesn't register the webrtc transport. A follow-up in 13c wires DCUtR.
> - The first-frame `open` envelope (`share_code`, `conn_id`) is a 13b↔13c contract — the 13c browser plan must match it exactly.

---

## Self-Review Notes

**Spec coverage check (against `docs/superpowers/specs/2026-04-16-slice13-libp2p-transport-design.md`, "Agent (`agent/`)" bullet):**
- ✅ `internal/peer/` removed (Task 9).
- ✅ `internal/transport/` new (Tasks 3–5).
- ✅ `signaling/client.go` receives relay multiaddr, no ICE (Task 6).
- ✅ `transfer/manager.go` unchanged (confirmed — no modification step for it).
- ✅ DataChannel adapter satisfies the existing interface (Task 4).

**Known follow-ups handled elsewhere:**
- 13b agent-side: `auth_ok` send is implemented (Task 7 Step 4), resolving 13a R2/R3.
- DCUtR transport is deliberately deferred to 13c (Task 11 PR note) — the agent Host in Task 5 uses `NoListenAddrs`, which means direct mode won't work even if the browser requests it. This is correct for 13b because only the browser (13c) drives the DCUtR handshake.

**Framing contract:** the open envelope `{"type":"open","share_code","conn_id"}` is not in the spec but is a direct consequence of libp2p streams not carrying session context. Document this in the 13c plan so the browser writes an identical envelope.
