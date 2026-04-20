# Secure Relay Plan C: Agent Relay Client + SecureRelayChannel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the agent-side secure relay transport: persist a long-lived agent Noise static key, advertise its public key during `register_share`, pre-register relay sessions from `relay_prepare`, and serve the existing transfer protocol over an agent-side `SecureRelayChannel`.

**Architecture:** Keep the current direct/WebRTC path intact, but add a parallel relay transport path that hangs off the existing signaling and transfer-manager seams. The agent persists one long-lived P-256 relay static key, sends its public key to the signaling server during share registration, opens a dedicated `/ws/relay` responder channel when the server issues `relay_prepare`, performs the Noise XX responder handshake, and then hands the encrypted text/binary channel to the existing `transfer.Manager`.

**Tech Stack:** Go, coder/websocket, Go stdlib `crypto/ecdh`, existing `agent/internal/noise`, existing `agent/internal/transfer`, PocketBase-backed signaling server contract

---

## File Structure

### New files

- `agent/internal/relaychannel/frame.go`
  - Agent-side relay frame constants and helpers for handshake (`0x00`), text (`0x01`), and binary (`0x02`) messages.
- `agent/internal/relaychannel/frame_test.go`
  - Verifies frame encode/decode behavior, unknown-kind rejection, and handshake/data size caps.
- `agent/internal/relaychannel/channel.go`
  - Agent-side `SecureRelayChannel`: connects to `/ws/relay`, sends hello token, runs the Noise XX responder handshake, then encrypts/decrypts framed text and binary messages while implementing the existing transfer `DataChannel` contract.
- `agent/internal/relaychannel/channel_test.go`
  - Handshake success, wrong-token/wrong-key failure, encrypted send/receive, and close-path tests using `httptest` + `coder/websocket`.
- `agent/internal/signaling/client_test.go`
  - Covers `register_share` relay static pub payloads, `relay_prepare` message parsing, and relay WebSocket URL derivation.

### Modified files

- `agent/internal/store/store.go`
  - Persist one long-lived relay static private key alongside the existing `agent_id`.
- `agent/internal/store/store_test.go`
  - Ensures the relay static key is generated once, persisted, and stable across reloads.
- `agent/internal/signaling/client.go`
  - Add `relay_static_pub` to `RegisterShare`, parse `relay_prepare`, expose relay WebSocket URL derivation, and keep the existing listener as the sole WebSocket reader.
- `agent/internal/daemon/daemon.go`
  - Add relay-static-key access to the store interface, track active relay channels per session, send `auth_ok` after HMAC success, and start agent relay channels on `relay_prepare`.
- `agent/internal/daemon/daemon_test.go`
  - Covers `auth_ok` emission, relay channel startup on `relay_prepare`, and cleanup behavior.
- `agent/cmd/agent/main.go`
  - Update the direct CLI path to send `relay_static_pub` during `register_share`.
- `signaling-server/internal/handler/agent_ws.go`
  - Include `code` in `relay_prepare` so the agent can bind a relay JWT to the right session.
- `signaling-server/internal/handler/agent_ws_test.go`
  - Assert `relay_prepare` now includes `code`.

## Task 1: Persist A Stable Agent Relay Static Key And Advertise Its Public Key During `register_share`

**Files:**
- Modify: `agent/internal/store/store.go`
- Modify: `agent/internal/store/store_test.go`
- Modify: `agent/internal/signaling/client.go`
- Create: `agent/internal/signaling/client_test.go`
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/daemon/daemon_test.go`
- Modify: `agent/cmd/agent/main.go`

- [ ] **Step 1: Write the failing store and signaling payload tests**

```go
// agent/internal/store/store_test.go
func TestGetRelayStaticPrivateKey_GeneratesAndPersistsStableKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	st, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	first, err := st.GetRelayStaticPrivateKey()
	if err != nil {
		t.Fatalf("GetRelayStaticPrivateKey() first call failed: %v", err)
	}
	second, err := st.GetRelayStaticPrivateKey()
	if err != nil {
		t.Fatalf("GetRelayStaticPrivateKey() second call failed: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("relay static private key changed within one store instance")
	}

	reloaded, err := New()
	if err != nil {
		t.Fatalf("New() reload failed: %v", err)
	}
	third, err := reloaded.GetRelayStaticPrivateKey()
	if err != nil {
		t.Fatalf("GetRelayStaticPrivateKey() after reload failed: %v", err)
	}
	if !bytes.Equal(first, third) {
		t.Fatal("relay static private key changed after reload")
	}
}
```

```go
// agent/internal/signaling/client_test.go
func TestRegisterShare_IncludesRelayStaticPub(t *testing.T) {
	var payload map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, helloBytes, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read hello: %v", err)
		}
		var hello map[string]any
		if err := json.Unmarshal(helloBytes, &hello); err != nil {
			t.Fatalf("Unmarshal hello: %v", err)
		}

		_, registerBytes, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read register_share: %v", err)
		}
		if err := json.Unmarshal(registerBytes, &payload); err != nil {
			t.Fatalf("Unmarshal register_share: %v", err)
		}

		resp, _ := json.Marshal(map[string]any{
			"type": "share_registered",
			"code": "SHARE1234",
		})
		_ = conn.Write(ctx, websocket.MessageText, resp)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client := New(wsURL, "key", "agent-1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- client.Listen(ctx) }()

	_, _, err := client.RegisterShare(ctx, "https://example.com/s/abc", "", false, "04abcd")
	if err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}

	if got := payload["relay_static_pub"]; got != "04abcd" {
		t.Fatalf("relay_static_pub = %v, want 04abcd", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
cd agent && go test ./internal/store ./internal/signaling -run 'TestGetRelayStaticPrivateKey_GeneratesAndPersistsStableKey|TestRegisterShare_IncludesRelayStaticPub' -v
```

Expected:

- FAIL because `GetRelayStaticPrivateKey` does not exist
- FAIL because `RegisterShare` does not accept or send `relay_static_pub`

- [ ] **Step 3: Write the minimal persistence and payload implementation**

```go
// agent/internal/store/store.go
type storeData struct {
	AgentID              string         `json:"agent_id"`
	RelayStaticPrivateHex string        `json:"relay_static_private_hex,omitempty"`
	Sessions             []SessionEntry `json:"sessions"`
}

func (s *Store) GetRelayStaticPrivateKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data.RelayStaticPrivateHex == "" {
		priv, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate relay static key: %w", err)
		}
		s.data.RelayStaticPrivateHex = hex.EncodeToString(priv.Bytes())
		if err := s.save(); err != nil {
			return nil, err
		}
		return append([]byte(nil), priv.Bytes()...), nil
	}

	raw, err := hex.DecodeString(s.data.RelayStaticPrivateHex)
	if err != nil {
		return nil, fmt.Errorf("decode relay static key: %w", err)
	}
	return raw, nil
}
```

```go
// agent/internal/signaling/client.go
type Message struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	PeerID    string          `json:"peer_id,omitempty"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	Err       string          `json:"message,omitempty"`
	Code      string          `json:"code,omitempty"`
	ConnID    string          `json:"conn_id,omitempty"`
	HMAC      string          `json:"hmac,omitempty"`
	Reconnected bool          `json:"reconnected,omitempty"`
	ICEServers []ICEServer    `json:"ice_servers,omitempty"`
	SID       string          `json:"sid,omitempty"`
	RelayJWT  string          `json:"relay_jwt,omitempty"`
	ExpiresAt string          `json:"expires_at,omitempty"`
}

func (c *Client) RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error) {
	responseCh := make(chan Message, 1)
	c.pendingRegMu.Lock()
	c.pendingReg = responseCh
	c.pendingRegMu.Unlock()
	defer func() {
		c.pendingRegMu.Lock()
		c.pendingReg = nil
		c.pendingRegMu.Unlock()
	}()

	msg := map[string]any{
		"type":             "register_share",
		"share_url":        shareURL,
		"relay_only":       relayOnly,
		"relay_static_pub": relayStaticPub,
	}
	if preferredCode != "" {
		msg["code"] = preferredCode
	}
	if err := c.Send(ctx, msg); err != nil {
		return "", false, fmt.Errorf("send register_share: %w", err)
	}

	select {
	case resp := <-responseCh:
		if resp.Type == "error" {
			return "", false, fmt.Errorf("server error: %s", resp.Err)
		}
		return resp.Code, resp.Reconnected, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}
```

```go
// agent/internal/daemon/daemon.go
type StoreInterface interface {
	GetAgentID() string
	GetRelayStaticPrivateKey() ([]byte, error)
	GetSession(code string) *store.SessionEntry
	GetByShareURL(shareURL string) *store.SessionEntry
	ListSessions(filterExpired bool) []store.SessionEntry
	SaveSession(session store.SessionEntry) error
	DeleteSession(code string) error
	IncrementDownloads(code string) (int, error)
}

type SignalingClientInterface interface {
	Connect(ctx context.Context) error
	RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error)
	DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error
	Send(ctx context.Context, msg any) error
	GetICEServers() []webrtc.ICEServer
	Listen(ctx context.Context) error
	SetOnMessage(handler func(signaling.Message))
}
```

```go
// agent/internal/daemon/daemon.go
func relayStaticPubHex(raw []byte) (string, error) {
	priv, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("import relay static key: %w", err)
	}
	return hex.EncodeToString(priv.PublicKey().Bytes()), nil
}
```

```go
// call sites in daemon.go and cmd/agent/main.go
relayPrivRaw, err := d.store.GetRelayStaticPrivateKey()
if err != nil {
	return "", fmt.Errorf("get relay static key: %w", err)
}
relayStaticPub, err := relayStaticPubHex(relayPrivRaw)
if err != nil {
	return "", err
}
code, reconnected, err := d.signaling.RegisterShare(ctx, shareURL, "", relayOnly, relayStaticPub)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
cd agent && go test ./internal/store ./internal/signaling -run 'TestGetRelayStaticPrivateKey_GeneratesAndPersistsStableKey|TestRegisterShare_IncludesRelayStaticPub' -v
```

Expected:

- PASS for both tests

- [ ] **Step 5: Commit**

```bash
git add agent/internal/store/store.go agent/internal/store/store_test.go agent/internal/signaling/client.go agent/internal/signaling/client_test.go agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go agent/cmd/agent/main.go
git commit -m "feat(agent): persist relay static identity and advertise it"
```

## Task 2: Extend Signaling Messages For `relay_prepare` And Include Session Context

**Files:**
- Modify: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/agent_ws_test.go`
- Modify: `agent/internal/signaling/client.go`
- Modify: `agent/internal/signaling/client_test.go`

- [ ] **Step 1: Write the failing relay_prepare parsing tests**

```go
// agent/internal/signaling/client_test.go
func TestListen_ParsesRelayPrepare(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, _, _ = conn.Read(ctx) // hello

		payload, _ := json.Marshal(map[string]any{
			"type":       "relay_prepare",
			"sid":        "sid-123",
			"code":       "SHARE123",
			"expires_at": "2026-04-19T12:00:00Z",
			"relay_jwt":  "relay.jwt.token",
		})
		_ = conn.Write(ctx, websocket.MessageText, payload)
		<-ctx.Done()
	}))
	defer server.Close()

	client := New("ws"+strings.TrimPrefix(server.URL, "http"), "key", "agent-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got := make(chan Message, 1)
	client.SetOnMessage(func(msg Message) {
		if msg.Type == "relay_prepare" {
			got <- msg
		}
	})

	go client.Listen(ctx)

	msg := <-got
	if msg.SID != "sid-123" || msg.Code != "SHARE123" || msg.RelayJWT != "relay.jwt.token" {
		t.Fatalf("relay_prepare parsed incorrectly: %+v", msg)
	}
}
```

```go
// signaling-server/internal/handler/agent_ws_test.go
func TestAgentWS_AuthOK_RelayPrepareIncludesSessionCode(t *testing.T) {
	// existing auth_ok setup, then:
	var relayPrepare map[string]any
	require.NoError(t, json.Unmarshal(agentRelayPrepare, &relayPrepare))
	assert.Equal(t, "relay_prepare", relayPrepare["type"])
	assert.Equal(t, "TESTCODE1", relayPrepare["code"])
	assert.NotEmpty(t, relayPrepare["sid"])
	assert.NotEmpty(t, relayPrepare["relay_jwt"])
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
cd agent && go test ./internal/signaling -run TestListen_ParsesRelayPrepare -v
cd ../signaling-server && go test ./internal/handler -run TestAgentWS_AuthOK_RelayPrepareIncludesSessionCode -v
```

Expected:

- FAIL because `relay_prepare` either does not include `code` or the agent client does not surface the new fields

- [ ] **Step 3: Make the message-shape changes**

```go
// signaling-server/internal/handler/agent_ws.go
hub.SendDirect(ctx, conn, map[string]any{
	"type":       "relay_prepare",
	"sid":        sid,
	"code":       msg.Code,
	"expires_at": now.Add(relay.TokenLifetime).Format(time.RFC3339),
	"relay_jwt":  agentJWT,
})
```

```go
// agent/internal/signaling/client.go
type Message struct {
	// existing fields...
	SID       string `json:"sid,omitempty"`
	Code      string `json:"code,omitempty"`
	RelayJWT  string `json:"relay_jwt,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
cd agent && go test ./internal/signaling -run TestListen_ParsesRelayPrepare -v
cd ../signaling-server && go test ./internal/handler -run TestAgentWS_AuthOK_RelayPrepareIncludesSessionCode -v
```

Expected:

- PASS for both tests

- [ ] **Step 5: Commit**

```bash
git add agent/internal/signaling/client.go agent/internal/signaling/client_test.go signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/agent_ws_test.go
git commit -m "feat(relay): include session context in relay prepare messages"
```

## Task 3: Implement Agent Relay Framing And The Go `SecureRelayChannel`

**Files:**
- Create: `agent/internal/relaychannel/frame.go`
- Create: `agent/internal/relaychannel/frame_test.go`
- Create: `agent/internal/relaychannel/channel.go`
- Create: `agent/internal/relaychannel/channel_test.go`

- [ ] **Step 1: Write the failing frame and channel tests**

```go
// agent/internal/relaychannel/frame_test.go
func TestWriteAndDecodeFrame(t *testing.T) {
	encoded, err := WriteFrame(FrameText, []byte("hello"))
	if err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	frame, rest, err := DecodeOneFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeOneFrame: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("expected no trailing bytes, got %d", len(rest))
	}
	if frame.Kind != FrameText || string(frame.Payload) != "hello" {
		t.Fatalf("decoded frame = %+v", frame)
	}
}

func TestDecodeOneFrame_RejectsUnknownKind(t *testing.T) {
	_, _, err := DecodeOneFrame([]byte{0xff, 0, 0, 0, 0})
	if err == nil || !strings.Contains(err.Error(), "unknown frame kind") {
		t.Fatalf("expected unknown frame kind error, got %v", err)
	}
}
```

```go
// agent/internal/relaychannel/channel_test.go
func TestSecureRelayChannel_HandshakeAndRoundTrip(t *testing.T) {
	serverStatic, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey(serverStatic): %v", err)
	}
	expectedResponderPub := serverStatic.PublicKey().Bytes()

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, helloBytes, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read hello: %v", err)
		}
		if !bytes.Contains(helloBytes, []byte(`"relay.jwt.token"`)) {
			t.Fatalf("unexpected hello payload: %s", string(helloBytes))
		}

		initiator, err := noise.NewInitiator()
		if err != nil {
			t.Fatalf("NewInitiator: %v", err)
		}
		msg1, err := initiator.WriteMessage1()
		if err != nil {
			t.Fatalf("WriteMessage1: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg1)); err != nil {
			t.Fatalf("Write msg1: %v", err)
		}

		_, msg2Frame, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read msg2: %v", err)
		}
		msg2, err := mustDecodeFramePayload(msg2Frame, FrameHandshake)
		if err != nil {
			t.Fatalf("Decode msg2: %v", err)
		}
		if err := initiator.ReadMessage2(msg2); err != nil {
			t.Fatalf("ReadMessage2: %v", err)
		}
		if !bytes.Equal(initiator.RemoteStaticPub(), expectedResponderPub) {
			t.Fatal("responder static public key mismatch")
		}

		msg3, err := initiator.WriteMessage3()
		if err != nil {
			t.Fatalf("WriteMessage3: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg3)); err != nil {
			t.Fatalf("Write msg3: %v", err)
		}

		iSend, iRecv := initiator.Split()
		textCipher, err := iSend.Encrypt(nil, []byte(`{"type":"list_request","path":""}`))
		if err != nil {
			t.Fatalf("Encrypt text: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameText, textCipher)); err != nil {
			t.Fatalf("Write encrypted text: %v", err)
		}

		_, replyFrame, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read encrypted reply: %v", err)
		}
		replyCipher, err := mustDecodeFramePayload(replyFrame, FrameText)
		if err != nil {
			t.Fatalf("Decode reply frame: %v", err)
		}
		replyPlain, err := iRecv.Decrypt(nil, replyCipher)
		if err != nil {
			t.Fatalf("Decrypt reply: %v", err)
		}
		if string(replyPlain) != `{"type":"hello"}` {
			t.Fatalf("reply plaintext = %s", replyPlain)
		}
	}))
	defer relayServer.Close()

	channel, err := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})
	if err != nil {
		t.Fatalf("NewSecureRelayChannel: %v", err)
	}

	gotMessages := make(chan []byte, 1)
	channel.SetOnMessage(func(data []byte) { gotMessages <- append([]byte(nil), data...) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := channel.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer channel.Close()

	if got := <-gotMessages; string(got) != `{"type":"list_request","path":""}` {
		t.Fatalf("got message = %s", got)
	}
	if err := channel.SendText(`{"type":"hello"}`); err != nil {
		t.Fatalf("SendText: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
cd agent && go test ./internal/relaychannel -run 'TestWriteAndDecodeFrame|TestSecureRelayChannel_HandshakeAndRoundTrip' -v
```

Expected:

- FAIL because the new package does not exist yet

- [ ] **Step 3: Write the frame codec and secure relay channel**

```go
// agent/internal/relaychannel/frame.go
package relaychannel

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	FrameHandshake byte = 0x00
	FrameText      byte = 0x01
	FrameBinary    byte = 0x02

	MaxHandshakePayload = 4 * 1024
	MaxFramePayload     = 8 * 1024 * 1024
)

type Frame struct {
	Kind    byte
	Payload []byte
}

func WriteFrame(kind byte, payload []byte) ([]byte, error) {
	cap, err := capForKind(kind)
	if err != nil {
		return nil, err
	}
	if len(payload) > cap {
		return nil, fmt.Errorf("frame too large: %d", len(payload))
	}
	out := make([]byte, 5+len(payload))
	out[0] = kind
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out, nil
}

func DecodeOneFrame(buf []byte) (Frame, []byte, error) {
	if len(buf) < 5 {
		return Frame{}, nil, errors.New("incomplete frame header")
	}
	cap, err := capForKind(buf[0])
	if err != nil {
		return Frame{}, nil, err
	}
	length := int(binary.BigEndian.Uint32(buf[1:5]))
	if length > cap {
		return Frame{}, nil, fmt.Errorf("frame too large: %d", length)
	}
	if len(buf) < 5+length {
		return Frame{}, nil, errors.New("incomplete frame payload")
	}
	frame := Frame{Kind: buf[0], Payload: append([]byte(nil), buf[5:5+length]...)}
	return frame, buf[5+length:], nil
}

func capForKind(kind byte) (int, error) {
	switch kind {
	case FrameHandshake:
		return MaxHandshakePayload, nil
	case FrameText, FrameBinary:
		return MaxFramePayload, nil
	default:
		return 0, fmt.Errorf("unknown frame kind: 0x%02x", kind)
	}
}
```

```go
// agent/internal/relaychannel/channel.go
package relaychannel

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/coder/websocket"
	"sharebridge/agent/internal/noise"
)

type SecureRelayConfig struct {
	RelayURL      string
	RelayJWT      string
	StaticPrivate *ecdh.PrivateKey
}

type SecureRelayChannel struct {
	cfg   SecureRelayConfig
	conn  *websocket.Conn
	noise *noise.NoiseXX
	send  *noise.CipherState
	recv  *noise.CipherState

	mu        sync.Mutex
	onMessage func([]byte)
	onOpen    func()
	onClose   func()
}

func NewSecureRelayChannel(cfg SecureRelayConfig) (*SecureRelayChannel, error) {
	if cfg.RelayURL == "" || cfg.RelayJWT == "" || cfg.StaticPrivate == nil {
		return nil, fmt.Errorf("relaychannel: missing required config")
	}
	return &SecureRelayChannel{cfg: cfg}, nil
}

func (c *SecureRelayChannel) SetOnMessage(handler func([]byte)) { c.onMessage = handler }
func (c *SecureRelayChannel) SetOnOpen(handler func())          { c.onOpen = handler }
func (c *SecureRelayChannel) SetOnClose(handler func())         { c.onClose = handler }

func (c *SecureRelayChannel) Start(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.cfg.RelayURL, nil)
	if err != nil {
		return fmt.Errorf("dial relay websocket: %w", err)
	}
	c.conn = conn

	hello, _ := json.Marshal(map[string]string{"token": c.cfg.RelayJWT})
	if err := c.conn.Write(ctx, websocket.MessageText, hello); err != nil {
		c.conn.CloseNow()
		return fmt.Errorf("write relay hello: %w", err)
	}

	nx, err := noise.NewResponder(c.cfg.StaticPrivate)
	if err != nil {
		c.conn.CloseNow()
		return fmt.Errorf("new responder: %w", err)
	}
	c.noise = nx

	if err := c.runHandshake(ctx); err != nil {
		c.conn.CloseNow()
		return err
	}

	go c.readLoop(context.Background())

	if c.onOpen != nil {
		c.onOpen()
	}
	return nil
}

func (c *SecureRelayChannel) runHandshake(ctx context.Context) error {
	_, msg1Frame, err := c.conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read msg1: %w", err)
	}
	msg1, err := decodeFramePayload(msg1Frame, FrameHandshake)
	if err != nil {
		return err
	}
	if err := c.noise.ReadMessage1(msg1); err != nil {
		return fmt.Errorf("read Noise msg1: %w", err)
	}

	msg2, err := c.noise.WriteMessage2()
	if err != nil {
		return fmt.Errorf("write Noise msg2: %w", err)
	}
	msg2Frame, err := WriteFrame(FrameHandshake, msg2)
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageBinary, msg2Frame); err != nil {
		return fmt.Errorf("write msg2 frame: %w", err)
	}

	_, msg3Frame, err := c.conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read msg3: %w", err)
	}
	msg3, err := decodeFramePayload(msg3Frame, FrameHandshake)
	if err != nil {
		return err
	}
	if err := c.noise.ReadMessage3(msg3); err != nil {
		return fmt.Errorf("read Noise msg3: %w", err)
	}

	c.send, c.recv = c.noise.Split()
	return nil
}

func (c *SecureRelayChannel) readLoop(ctx context.Context) {
	defer func() {
		if c.onClose != nil {
			c.onClose()
		}
	}()

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		frame, _, err := DecodeOneFrame(data)
		if err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, "invalid relay frame")
			return
		}
		if frame.Kind != FrameText && frame.Kind != FrameBinary {
			c.conn.Close(websocket.StatusPolicyViolation, "unexpected relay frame kind")
			return
		}
		plain, err := c.recv.Decrypt(nil, frame.Payload)
		if err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, "relay decrypt failed")
			return
		}
		if c.onMessage != nil {
			c.onMessage(plain)
		}
	}
}

func (c *SecureRelayChannel) SendText(text string) error {
	return c.sendFrame(FrameText, []byte(text))
}

func (c *SecureRelayChannel) SendBinary(data []byte) error {
	return c.sendFrame(FrameBinary, data)
}

func (c *SecureRelayChannel) BufferedAmount() uint64 { return 0 }

func (c *SecureRelayChannel) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close(websocket.StatusNormalClosure, "relay closed")
}

func (c *SecureRelayChannel) sendFrame(kind byte, plaintext []byte) error {
	ciphertext, err := c.send.Encrypt(nil, plaintext)
	if err != nil {
		return err
	}
	frame, err := WriteFrame(kind, ciphertext)
	if err != nil {
		return err
	}
	return c.conn.Write(context.Background(), websocket.MessageBinary, frame)
}

func decodeFramePayload(raw []byte, wantKind byte) ([]byte, error) {
	frame, rest, err := DecodeOneFrame(raw)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("unexpected trailing frame bytes")
	}
	if frame.Kind != wantKind {
		return nil, fmt.Errorf("unexpected frame kind: got 0x%02x want 0x%02x", frame.Kind, wantKind)
	}
	return frame.Payload, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
cd agent && go test ./internal/relaychannel -run 'TestWriteAndDecodeFrame|TestSecureRelayChannel_HandshakeAndRoundTrip' -v
```

Expected:

- PASS for the new relaychannel tests

- [ ] **Step 5: Commit**

```bash
git add agent/internal/relaychannel/frame.go agent/internal/relaychannel/frame_test.go agent/internal/relaychannel/channel.go agent/internal/relaychannel/channel_test.go
git commit -m "feat(agent): add secure relay channel responder"
```

## Task 4: Integrate `relay_prepare` Into The Daemon And Serve Transfers Over Relay

**Files:**
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/daemon/daemon_test.go`
- Modify: `agent/internal/signaling/client.go`
- Modify: `agent/internal/signaling/client_test.go`

- [ ] **Step 1: Write the failing daemon relay tests**

```go
// agent/internal/daemon/daemon_test.go
func TestHandleJoin_SendsAuthOKAfterHMACVerification(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sig)
	if err != nil {
		t.Fatalf("NewWithSignaling: %v", err)
	}

	d.sessions["SHARE123"] = &Session{
		Code:      "SHARE123",
		Password:  "secret",
		CreatedAt: time.Now(),
		peers:     make(map[string]*peer.Peer),
	}

	d.nonces["conn-1"] = nonceEntry{
		nonce:     "abc123",
		expiresAt: time.Now().Add(time.Minute),
	}

	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte("abc123"))
	joinedHMAC := hex.EncodeToString(mac.Sum(nil))

	d.handleJoin("conn-1", "SHARE123", joinedHMAC)

	if !hasSentMessage(sig.sendMessages, "auth_ok", map[string]any{
		"conn_id": "conn-1",
		"code":    "SHARE123",
	}) {
		t.Fatalf("expected auth_ok to be sent, got %#v", sig.sendMessages)
	}
}

func TestHandleRelayPrepare_StartsRelayTransferChannel(t *testing.T) {
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	cfgMgr := &mockConfigManager{cfg: cfg}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())

	d, err := NewWithSignaling(cfgMgr, st, sig)
	if err != nil {
		t.Fatalf("NewWithSignaling: %v", err)
	}

	session := &Session{
		Code:      "SHARE123",
		CreatedAt: time.Now(),
		peers:     make(map[string]*peer.Peer),
		relayChannels: make(map[string]relayTransferChannel),
	}
	d.sessions["SHARE123"] = session

	started := make(chan struct{}, 1)
	d.newRelayChannel = func(cfg relayChannelConfig) (relayTransferChannel, error) {
		return &mockRelayChannel{
			startFn: func(context.Context) error {
				started <- struct{}{}
				return nil
			},
		}, nil
	}

	d.handleSignalingMessage(signaling.Message{
		Type:     "relay_prepare",
		SID:      "sid-123",
		Code:     "SHARE123",
		RelayJWT: "relay.jwt.token",
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("relay channel never started")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
cd agent && go test ./internal/daemon -run 'TestHandleJoin_SendsAuthOKAfterHMACVerification|TestHandleRelayPrepare_StartsRelayTransferChannel' -v
```

Expected:

- FAIL because `handleJoin` does not send `auth_ok`
- FAIL because the daemon ignores `relay_prepare`

- [ ] **Step 3: Add relay channel integration with minimal daemon plumbing**

```go
// agent/internal/daemon/daemon.go
type relayTransferChannel interface {
	Start(ctx context.Context) error
	SendBinary(data []byte) error
	SendText(text string) error
	BufferedAmount() uint64
	Close() error
	SetOnMessage(handler func([]byte))
	SetOnOpen(handler func())
	SetOnClose(handler func())
}

type Session struct {
	Code         string
	ShareURL     string
	ShareType    string
	FileID       string
	Password     string
	ExpiresAt    time.Time
	MaxDownloads int
	Downloads    int
	RelayOnly    bool
	CreatedAt    time.Time

	webdavClient  *cloudwebdav.Client
	peers         map[string]*peer.Peer
	relayChannels map[string]relayTransferChannel
	mu            sync.Mutex
}

type Daemon struct {
	// existing fields...
	newRelayChannel func(cfg relayChannelConfig) (relayTransferChannel, error)
}

type relayChannelConfig struct {
	RelayURL      string
	RelayJWT      string
	StaticPrivate []byte
}
```

```go
// in New / NewWithSignaling
d.newRelayChannel = func(cfg relayChannelConfig) (relayTransferChannel, error) {
	priv, err := ecdh.P256().NewPrivateKey(cfg.StaticPrivate)
	if err != nil {
		return nil, err
	}
	return relaychannel.NewSecureRelayChannel(relaychannel.SecureRelayConfig{
		RelayURL:      cfg.RelayURL,
		RelayJWT:      cfg.RelayJWT,
		StaticPrivate: priv,
	})
}
```

```go
// agent/internal/daemon/daemon.go
func (d *Daemon) handleSignalingMessage(msg signaling.Message) {
	switch msg.Type {
	case "welcome":
		// existing
	case "knock":
		go d.handleKnock(msg.ConnID, msg.Code)
	case "join":
		go d.handleJoin(msg.ConnID, msg.Code, msg.HMAC)
	case "relay_prepare":
		go d.handleRelayPrepare(msg)
	case "answer":
		d.handleAnswer(msg.PeerID, msg.SDP)
	case "ice_candidate":
		d.handleICECandidate(msg.PeerID, msg.Candidate)
	case "error":
		log.Printf("signaling error: %s", msg.Err)
	}
}
```

```go
// after successful HMAC verification in handleJoin
d.signaling.Send(context.Background(), map[string]any{
	"type":    "auth_ok",
	"conn_id": connID,
	"code":    sessionCode,
})
go d.createPeer(connID, sessionCode)
```

```go
// new helper in daemon.go
func (d *Daemon) handleRelayPrepare(msg signaling.Message) {
	d.mu.RLock()
	session := d.sessions[msg.Code]
	d.mu.RUnlock()
	if session == nil {
		log.Printf("relay_prepare for unknown session %s", msg.Code)
		return
	}

	rawPriv, err := d.store.GetRelayStaticPrivateKey()
	if err != nil {
		log.Printf("get relay static key: %v", err)
		return
	}

	channel, err := d.newRelayChannel(relayChannelConfig{
		RelayURL:      signaling.RelayWebSocketURL(d.config.SignalingURL),
		RelayJWT:      msg.RelayJWT,
		StaticPrivate: rawPriv,
	})
	if err != nil {
		log.Printf("new relay channel: %v", err)
		return
	}

	tm := transfer.NewManager(channel, session.webdavClient, session.MaxDownloads)
	tm.SetDownloadCount(session.Downloads)
	tm.OnSessionExpired = func() {
		_ = d.signaling.Send(context.Background(), map[string]any{
			"type":       "session_expired",
			"session_id": session.Code,
		})
	}
	tm.OnDownloadComplete = func(bytesTransferred int64) {
		session.mu.Lock()
		session.Downloads++
		session.mu.Unlock()
		if _, err := d.store.IncrementDownloads(session.Code); err != nil {
			log.Printf("persist download count: %v", err)
		}
		_ = d.signaling.DownloadComplete(context.Background(), session.Code, bytesTransferred)
	}

	channel.SetOnMessage(tm.HandleMessage)
	channel.SetOnOpen(tm.HandleOpen)
	channel.SetOnClose(func() {
		session.mu.Lock()
		delete(session.relayChannels, msg.SID)
		session.mu.Unlock()
	})

	session.mu.Lock()
	if session.relayChannels == nil {
		session.relayChannels = make(map[string]relayTransferChannel)
	}
	session.relayChannels[msg.SID] = channel
	session.mu.Unlock()

	if err := channel.Start(context.Background()); err != nil {
		log.Printf("start relay channel sid=%s: %v", msg.SID, err)
		session.mu.Lock()
		delete(session.relayChannels, msg.SID)
		session.mu.Unlock()
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```bash
cd agent && go test ./internal/daemon -run 'TestHandleJoin_SendsAuthOKAfterHMACVerification|TestHandleRelayPrepare_StartsRelayTransferChannel' -v
```

Expected:

- PASS for both tests

- [ ] **Step 5: Commit**

```bash
git add agent/internal/daemon/daemon.go agent/internal/daemon/daemon_test.go
git commit -m "feat(agent): pre-register secure relay channels from signaling"
```

## Task 5: Run Cross-Package Regression Coverage And Clean Up The Legacy CLI Path

**Files:**
- Modify: `agent/cmd/agent/main.go`
- Modify: `agent/internal/signaling/client.go`
- Modify: `agent/internal/daemon/daemon.go`
- Modify: `agent/internal/transfer/manager_test.go` (only if a small helper is needed for relay-channel coverage)

- [ ] **Step 1: Write one targeted regression test for relay WebSocket URL derivation**

```go
// agent/internal/signaling/client_test.go
func TestRelayWebSocketURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "wss://sharebridge.app", want: "wss://sharebridge.app/ws/relay"},
		{in: "ws://localhost:8787", want: "ws://localhost:8787/ws/relay"},
	}

	for _, tc := range cases {
		if got := RelayWebSocketURL(tc.in); got != tc.want {
			t.Fatalf("RelayWebSocketURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the targeted regression test to verify it fails**

Run:

```bash
cd agent && go test ./internal/signaling -run TestRelayWebSocketURL -v
```

Expected:

- FAIL because `relayWebSocketURL` does not exist yet

- [ ] **Step 3: Implement the helper and update the remaining CLI call site**

```go
// agent/internal/signaling/client.go
func RelayWebSocketURL(signalingURL string) string {
	u, err := url.Parse(signalingURL)
	if err != nil {
		return signalingURL + "/ws/relay"
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/ws/relay"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
```

```go
// agent/cmd/agent/main.go
relayPrivRaw, err := st.GetRelayStaticPrivateKey()
if err != nil {
	return "", fmt.Errorf("get relay static key: %w", err)
}
relayStaticPub, err := relayStaticPubHex(relayPrivRaw)
if err != nil {
	return "", err
}
code, reconnected, err := sig.RegisterShare(ctx, shareURL, preferredCode, relayOnly, relayStaticPub)
```

- [ ] **Step 4: Run the full agent regression suite**

Run:

```bash
cd agent && go test ./internal/store ./internal/signaling ./internal/relaychannel ./internal/daemon ./internal/transfer ./cmd/agent/... -v
```

Expected:

- PASS across the touched packages

- [ ] **Step 5: Commit**

```bash
git add agent/internal/signaling/client.go agent/internal/signaling/client_test.go agent/cmd/agent/main.go
git commit -m "test(agent): lock down relay client regression coverage"
```

## Spec Coverage Check

- Stable agent Noise static key persistence: covered by Task 1.
- `register_share` advertising of `relay_static_pub`: covered by Task 1.
- `relay_prepare` parsing and additive server payload shape: covered by Task 2.
- Agent-side relay framing and Noise XX responder handshake: covered by Task 3.
- Agent-side `SecureRelayChannel` implementing the transfer `DataChannel` contract: covered by Task 3.
- Daemon integration with the existing `transfer.Manager`: covered by Task 4.
- Direct path left intact while relay path is additive: covered by Tasks 4 and 5.

## Self-Review

- No placeholder steps remain; every task has concrete files, code, tests, and commands.
- Type names are consistent across tasks:
  - `GetRelayStaticPrivateKey`
  - `RegisterShare(... relayStaticPub string)`
  - `relay_prepare`
  - `SecureRelayChannel`
- One additive backend dependency is explicit in this plan: `relay_prepare` must include `code`, otherwise the agent cannot bind a relay JWT to the correct session.
