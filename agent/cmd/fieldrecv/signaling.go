package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/coder/websocket"
)

// iceServer mirrors the server's ice_config entry.
type iceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// browserMessage is the union of fields fieldrecv reads or writes on the
// browser-side (/ws/client) signaling socket.
type browserMessage struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"session_id,omitempty"`
	PeerID      string          `json:"peer_id,omitempty"`
	SDP         string          `json:"sdp,omitempty"`
	Candidate   json.RawMessage `json:"candidate,omitempty"`
	HMAC        string          `json:"hmac,omitempty"`
	Value       string          `json:"value,omitempty"`
	HasPassword bool            `json:"has_password,omitempty"`
	RelayOnly   bool            `json:"relay_only,omitempty"`
	Message     string          `json:"message,omitempty"`
	Scope       string          `json:"scope,omitempty"`
	ICEServers  []iceServer     `json:"ice_servers,omitempty"`
}

// browserClient is a minimal browser-role signaling WebSocket client. All
// writes are serialized because ICE callbacks and the main loop share it.
type browserClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

// dialBrowser connects to the browser signaling endpoint. baseURL may be a
// bare origin (wss://host) or a full /ws/client URL. code is the share code.
func dialBrowser(ctx context.Context, baseURL, code string, insecure bool) (*browserClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse signaling url: %w", err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/ws/client"
	}
	q := u.Query()
	q.Set("session", code)
	u.RawQuery = q.Encode()

	opts := &websocket.DialOptions{}
	if insecure {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}
	conn, _, err := websocket.Dial(ctx, u.String(), opts)
	if err != nil {
		return nil, fmt.Errorf("dial signaling server: %w", err)
	}
	return &browserClient{conn: conn}, nil
}

func (c *browserClient) send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal signaling message: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Write(ctx, websocket.MessageText, data)
}

func (c *browserClient) read(ctx context.Context) (browserMessage, error) {
	var msg browserMessage
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return msg, err
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return msg, fmt.Errorf("decode signaling message %q: %w", string(data), err)
	}
	return msg, nil
}

func (c *browserClient) ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Ping(ctx)
}

func (c *browserClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.CloseNow()
}

// joinHMAC computes the browser's HMAC-SHA256(password, nonce) proof, hex
// encoded. Empty password yields an empty string (password-less shares).
func joinHMAC(password, nonce string) string {
	if password == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}
