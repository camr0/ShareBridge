package signaling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"

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
	SessionID   string          `json:"session_id,omitempty"`  // Share code
	PeerID      string          `json:"peer_id,omitempty"`     // Unique peer connection ID
	SDP         string          `json:"sdp,omitempty"`
	Candidate   json.RawMessage `json:"candidate,omitempty"`
	Err         string          `json:"message,omitempty"`
	Code        string          `json:"code,omitempty"`
	Reconnected bool            `json:"reconnected,omitempty"`
	ICEServers  []ICEServer     `json:"ice_servers,omitempty"`
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

// RegisterShare sends register_share message and waits for response.
func (c *Client) RegisterShare(ctx context.Context, shareURL, preferredCode string) (string, bool, error) {
	msg := map[string]string{
		"type":      "register_share",
		"share_url": shareURL,
	}
	if preferredCode != "" {
		msg["code"] = preferredCode
	}

	if err := c.Send(ctx, msg); err != nil {
		return "", false, fmt.Errorf("send register_share: %w", err)
	}

	// Wait for response
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return "", false, fmt.Errorf("read response: %w", err)
		}

		var resp Message
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}

		switch resp.Type {
		case "welcome":
			// Store ICE servers from welcome message
			if len(resp.ICEServers) > 0 {
				c.mu.Lock()
				c.iceServers = make([]webrtc.ICEServer, len(resp.ICEServers))
				for i, s := range resp.ICEServers {
					c.iceServers[i] = webrtc.ICEServer{
						URLs:       s.URLs,
						Username:   s.Username,
						Credential: s.Credential,
					}
				}
				c.mu.Unlock()
			}
			// Continue waiting for share_registered
			continue
		case "share_registered":
			return resp.Code, resp.Reconnected, nil
		case "error":
			return "", false, fmt.Errorf("server error: %s", resp.Err)
		default:
			// Unexpected message type - ignore and continue waiting
			continue
		}
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

// GetICEServers returns the ICE servers received from the server's welcome message.
func (c *Client) GetICEServers() []webrtc.ICEServer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.iceServers
}

// Listen reads messages in a loop and calls OnMessage for each one.
func (c *Client) Listen(ctx context.Context) error {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return err
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if c.OnMessage != nil {
			c.OnMessage(msg)
		}
	}
}
