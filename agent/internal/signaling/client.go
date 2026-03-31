package signaling

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// Message is any message received from the signaling server.
type Message struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	Err       string          `json:"message,omitempty"`
}

// Client manages a WebSocket connection to the signaling server.
type Client struct {
	serverURL string // ws:// or wss://
	token     string
	conn      *websocket.Conn
	OnMessage func(msg Message)
}

func New(serverURL, token string) *Client {
	return &Client{serverURL: serverURL, token: token}
}

// Connect dials the signaling server and sends the register message.
func (c *Client) Connect(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.serverURL+"/ws/agent", nil)
	if err != nil {
		return fmt.Errorf("dial signaling server: %w", err)
	}
	c.conn = conn
	return c.Send(ctx, map[string]string{"type": "register", "token": c.token})
}

// CreateSession calls POST /api/v1/sessions and returns the session code.
func (c *Client) CreateSession(ctx context.Context, shareURL, preferredCode string) (string, error) {
	reqBody := map[string]string{"share_url": shareURL}
	if preferredCode != "" {
		reqBody["preferred_code"] = preferredCode
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpBase := strings.NewReplacer("ws://", "http://", "wss://", "https://").Replace(c.serverURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpBase+"/api/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create session request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var e struct{ Error string `json:"error"` }
		json.NewDecoder(resp.Body).Decode(&e)
		return "", fmt.Errorf("server returned %d: %s", resp.StatusCode, e.Error)
	}

	var result struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return result.Code, nil
}

// Send serializes msg as JSON and writes it to the WebSocket.
func (c *Client) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	return c.conn.Write(ctx, websocket.MessageText, data)
}

// Listen reads messages in a loop and calls OnMessage for each one.
// Returns when the connection is closed or ctx is cancelled.
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
