package signaling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// ICEServer represents an ICE server configuration from the signaling server.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Message is any message received from the signaling server.
type Message struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"session_id,omitempty"` // Share code
	PeerID      string          `json:"peer_id,omitempty"`    // Unique peer connection ID
	SDP         string          `json:"sdp,omitempty"`
	Candidate   json.RawMessage `json:"candidate,omitempty"`
	Err         string          `json:"message,omitempty"`
	Code        string          `json:"code,omitempty"`
	ConnID      string          `json:"conn_id,omitempty"`
	HMAC        string          `json:"hmac,omitempty"`
	Reconnected bool            `json:"reconnected,omitempty"`
	ICEServers  []ICEServer     `json:"ice_servers,omitempty"`
	SID         string          `json:"sid,omitempty"`
	RelayJWT    string          `json:"relay_jwt,omitempty"`
	ExpiresAt   string          `json:"expires_at,omitempty"`
}

type RegisterShareOptions struct {
	ShareURL            string
	PreferredCode       string
	ShareType           string
	IsPasswordProtected bool
	RelayOnly           bool
	RelayStaticPub      string
}

// Client manages a WebSocket connection to the signaling server.
type Client struct {
	serverURL  string
	apiKey     string
	agentID    string
	conn       *websocket.Conn
	OnMessage  func(msg Message)
	mu         sync.Mutex
	iceServers []webrtc.ICEServer // Store ICE config from server

	// pendingReg receives the share_registered (or error) response for the
	// RegisterShare call currently in flight. nil when no registration pending.
	// All WebSocket reads go through Listen, so RegisterShare must not call
	// conn.Read directly while Listen is running.
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

	// Send hello message
	if err := c.Send(ctx, map[string]string{
		"type":     "hello",
		"version":  "1.0",
		"agent_id": c.agentID,
	}); err != nil {
		conn.CloseNow()
		return fmt.Errorf("send hello: %w", err)
	}

	return nil
}

// RegisterShare sends register_share message and waits for the response.
// It must not call conn.Read directly — all reads go through Listen.
// The response is delivered via pendingReg, which Listen feeds.
// relayStaticPub is the hex-encoded P-256 public key for relay identity.
func (c *Client) RegisterShare(ctx context.Context, shareURL, preferredCode string, relayOnly bool, relayStaticPub string) (string, bool, error) {
	return c.RegisterShareWithOptions(ctx, RegisterShareOptions{
		ShareURL:       shareURL,
		PreferredCode:  preferredCode,
		RelayOnly:      relayOnly,
		RelayStaticPub: relayStaticPub,
	})
}

func (c *Client) RegisterShareWithOptions(ctx context.Context, opts RegisterShareOptions) (string, bool, error) {
	responseCh := make(chan Message, 1)
	c.pendingRegMu.Lock()
	if c.pendingReg != nil {
		c.pendingRegMu.Unlock()
		return "", false, fmt.Errorf("registration already in progress")
	}
	c.pendingReg = responseCh
	c.pendingRegMu.Unlock()
	defer func() {
		c.pendingRegMu.Lock()
		c.pendingReg = nil
		c.pendingRegMu.Unlock()
	}()

	msg := map[string]any{
		"type":       "register_share",
		"share_url":  opts.ShareURL,
		"relay_only": opts.RelayOnly,
	}
	if opts.PreferredCode != "" {
		msg["code"] = opts.PreferredCode
	}
	if opts.ShareType != "" {
		msg["share_type"] = opts.ShareType
	}
	if opts.IsPasswordProtected {
		msg["is_password_protected"] = true
	}
	if opts.RelayStaticPub != "" {
		msg["relay_static_pub"] = opts.RelayStaticPub
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

func (c *Client) UnregisterShare(ctx context.Context, code string) error {
	return c.Send(ctx, map[string]string{"type": "unregister_share", "code": code})
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

// GetICEServers returns the ICE servers received from the server's welcome message.
func (c *Client) GetICEServers() []webrtc.ICEServer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.iceServers
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

		// Update ICE servers whenever the server sends a welcome.
		if msg.Type == "welcome" && len(msg.ICEServers) > 0 {
			c.mu.Lock()
			c.iceServers = make([]webrtc.ICEServer, len(msg.ICEServers))
			for i, s := range msg.ICEServers {
				c.iceServers[i] = webrtc.ICEServer{
					URLs:       s.URLs,
					Username:   s.Username,
					Credential: s.Credential,
				}
			}
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

// RelayWebSocketURL derives the relay WebSocket URL from the signaling URL.
// It converts http/https to ws/wss and appends "/ws/relay" path.
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
