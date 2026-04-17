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
	peerID    string // libp2p peer ID sent in hello so the server can register it in the relay registry
	conn      *websocket.Conn
	OnMessage func(msg Message)
	mu        sync.Mutex

	relayMultiaddr string

	pendingReg   chan Message
	pendingRegMu sync.Mutex
}

func New(serverURL, apiKey, agentID string) *Client {
	return &Client{
		serverURL: serverURL,
		apiKey:    apiKey,
		agentID:   agentID,
	}
}

// SetPeerID sets the libp2p peer ID to include in the hello message.
// Must be called before Connect.
func (c *Client) SetPeerID(peerID string) {
	c.peerID = peerID
}

// Connect dials the signaling server with API key auth and sends hello.
func (c *Client) Connect(ctx context.Context) error {
	// WebSocket URL with api_key query param
	params := url.Values{}
	params.Set("api_key", c.apiKey)
	wsURL := c.serverURL + "/ws/agent?" + params.Encode()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial signaling server: %w", err)
	}
	c.conn = conn

	hello := map[string]string{
		"type":     "hello",
		"version":  "1.0",
		"agent_id": c.agentID,
	}
	if c.peerID != "" {
		hello["peer_id"] = c.peerID
	}
	if err := c.Send(ctx, hello); err != nil {
		conn.CloseNow()
		return fmt.Errorf("send hello: %w", err)
	}

	return nil
}

// RegisterShare sends register_share message and waits for the response.
// It must not call conn.Read directly — all reads go through Listen.
// The response is delivered via pendingReg, which Listen feeds.
func (c *Client) RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool) (string, bool, error) {
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
		"type":       "register_share",
		"share_url":  shareURL,
		"relay_only": relayOnly,
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

// DownloadComplete notifies server of completed download.
func (c *Client) DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error {
	return c.Send(ctx, map[string]any{
		"type":              "download_complete",
		"code":              code,
		"bytes_transferred": bytesTransferred,
	})
}

// Send serializes msg as JSON and writes it to the WebSocket.
func (c *Client) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	return c.conn.Write(ctx, websocket.MessageText, data)
}

// GetRelayMultiaddr returns the relay's libp2p multiaddr received in welcome.
func (c *Client) GetRelayMultiaddr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.relayMultiaddr
}

// Listen reads messages in a loop and dispatches them.
// It is the sole reader of the WebSocket connection — RegisterShare and
// other callers must not call conn.Read concurrently.
func (c *Client) Listen(ctx context.Context) error {
	// Keepalive ping: sends every 30s to prevent Cloudflare's 100s WebSocket timeout
	// from disconnecting idle agents. Any WebSocket frame resets the timeout.
	keepaliveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.conn.Ping(keepaliveCtx); err != nil {
					return // Connection closed or error
				}
			case <-keepaliveCtx.Done():
				return
			}
		}
	}()

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return err
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		// Store relay multiaddr from welcome message.
		if msg.Type == "welcome" && msg.RelayMultiaddr != "" {
			c.mu.Lock()
			c.relayMultiaddr = msg.RelayMultiaddr
			c.mu.Unlock()
		}

		// Route registration responses to RegisterShare if one is in flight.
		if msg.Type == "share_registered" || msg.Type == "error" {
			c.pendingRegMu.Lock()
			ch := c.pendingReg
			c.pendingRegMu.Unlock()
			if ch != nil {
				ch <- msg
				continue
			}
		}

		if c.OnMessage != nil {
			c.OnMessage(msg)
		}
	}
}

// SetOnMessage sets the message handler callback.
func (c *Client) SetOnMessage(handler func(Message)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.OnMessage = handler
}
