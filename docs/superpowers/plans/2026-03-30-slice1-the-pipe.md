# Slice 1 — The Pipe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Get a WebRTC DataChannel open between the agent and a browser via the signaling server — both sides log "DataChannel open", no file transfer yet.

**Architecture:** Agent connects to signaling server via WebSocket and creates a session. Browser joins using the session code, triggering the agent to generate a WebRTC offer. SDP and ICE candidates are relayed through the signaling server until the DataChannel opens P2P. STUN-only ICE (no Coturn yet).

**Tech Stack:** Go 1.22, Gin, github.com/coder/websocket, github.com/pion/webrtc/v4, Vanilla JS

---

## File Map

```
signaling-server/
├── go.mod                              module opencloudshare/server
├── cmd/server/main.go                  wire gin + deps, start server
├── internal/config/config.go          env var loading (PORT, AUTH_TOKEN, STUN_URL)
├── internal/session/manager.go        in-memory session store (create, get, delete, expiry)
├── internal/session/manager_test.go
├── internal/hub/hub.go                tracks agent WS conns + session pairings, routes msgs
├── internal/handler/routes.go         register all gin routes
├── internal/handler/rest.go           POST /api/v1/sessions
├── internal/handler/agent_ws.go       WS /ws/agent — register, relay offer/ICE
└── internal/handler/browser_ws.go     WS /ws/client — join, relay answer/ICE
web/
├── index.html                         session code entry form
└── app.js                             WebRTC answer + DataChannel open handler

agent/
├── go.mod                             module opencloudshare/agent
├── cmd/agent/main.go                  connect, create session, handle signaling events
├── internal/config/config.go          env var loading (SIGNALING_SERVER, AUTH_TOKEN, SHARE_URL)
├── internal/signaling/client.go       WS client: connect, register, send, listen
└── internal/peer/peer.go              pion RTCPeerConnection: offer, answer, ICE, DataChannel
```

---

## Task 1: Scaffold monorepo

**Files:**
- Create: `signaling-server/go.mod`
- Create: `agent/go.mod`

- [ ] **Step 1: Create directory structure**

```bash
cd /Users/ali/Git/OpenCloudShare

mkdir -p signaling-server/cmd/server
mkdir -p signaling-server/internal/{config,session,hub,handler}
mkdir -p signaling-server/web

mkdir -p agent/cmd/agent
mkdir -p agent/internal/{config,signaling,peer}
```

- [ ] **Step 2: Initialize signaling server module and install deps**

```bash
cd signaling-server
go mod init opencloudshare/server
go get github.com/gin-gonic/gin@latest
go get github.com/coder/websocket@latest
go get github.com/stretchr/testify@latest
go mod tidy
cd ..
```

- [ ] **Step 3: Initialize agent module and install deps**

```bash
cd agent
go mod init opencloudshare/agent
go get github.com/coder/websocket@latest
go get github.com/pion/webrtc/v4@latest
go mod tidy
cd ..
```

- [ ] **Step 4: Verify both modules compile**

```bash
cd signaling-server && go build ./... && cd ..
cd agent && go build ./... && cd ..
```

Expected: no output (both compile cleanly with no source files yet — `go build ./...` is a no-op on an empty module).

- [ ] **Step 5: Commit**

```bash
git add signaling-server/go.mod signaling-server/go.sum agent/go.mod agent/go.sum
git commit -m "chore: scaffold signaling-server and agent go modules"
```

---

## Task 2: Session manager

**Files:**
- Create: `signaling-server/internal/session/manager.go`
- Create: `signaling-server/internal/session/manager_test.go`

- [ ] **Step 1: Write failing tests**

```go
// signaling-server/internal/session/manager_test.go
package session_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"opencloudshare/server/internal/session"
)

func TestCreate_ReturnsSessionWithEightCharCode(t *testing.T) {
	m := session.NewManager()
	s, err := m.Create("tok", "https://example.com/s/ABC", 24*time.Hour)
	require.NoError(t, err)
	assert.Len(t, s.ID, 8)
	assert.Equal(t, "tok", s.Token)
	assert.Equal(t, "https://example.com/s/ABC", s.ShareURL)
	assert.True(t, s.ExpiresAt.After(time.Now()))
}

func TestCreate_CodesAreUnique(t *testing.T) {
	m := session.NewManager()
	codes := make(map[string]bool)
	for i := 0; i < 100; i++ {
		s, err := m.Create("tok", "url", time.Hour)
		require.NoError(t, err)
		codes[s.ID] = true
	}
	assert.Len(t, codes, 100)
}

func TestGet_ReturnsExistingSession(t *testing.T) {
	m := session.NewManager()
	s, _ := m.Create("tok", "url", time.Hour)
	got, ok := m.Get(s.ID)
	assert.True(t, ok)
	assert.Equal(t, s.ID, got.ID)
}

func TestGet_ReturnsFalseForExpired(t *testing.T) {
	m := session.NewManager()
	s, _ := m.Create("tok", "url", -time.Second) // already expired
	_, ok := m.Get(s.ID)
	assert.False(t, ok)
}

func TestGet_ReturnsFalseForUnknown(t *testing.T) {
	m := session.NewManager()
	_, ok := m.Get("notexist")
	assert.False(t, ok)
}

func TestDelete_RemovesSession(t *testing.T) {
	m := session.NewManager()
	s, _ := m.Create("tok", "url", time.Hour)
	m.Delete(s.ID)
	_, ok := m.Get(s.ID)
	assert.False(t, ok)
}
```

- [ ] **Step 2: Run tests — confirm they fail**

```bash
cd signaling-server && go test ./internal/session/... 2>&1
```

Expected: `cannot find package "opencloudshare/server/internal/session"` or similar compile error.

- [ ] **Step 3: Implement session manager**

```go
// signaling-server/internal/session/manager.go
package session

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sync"
	"time"
)

const codeChars = "abcdefghijklmnopqrstuvwxyz0123456789"
const codeLen = 8

type Session struct {
	ID        string
	Token     string
	ShareURL  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewManager() *Manager {
	return &Manager{sessions: make(map[string]*Session)}
}

func (m *Manager) Create(token, shareURL string, ttl time.Duration) (*Session, error) {
	id, err := generateCode()
	if err != nil {
		return nil, fmt.Errorf("generate code: %w", err)
	}
	s := &Session{
		ID:        id,
		Token:     token,
		ShareURL:  shareURL,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok || time.Now().After(s.ExpiresAt) {
		return nil, false
	}
	return s, true
}

func (m *Manager) Delete(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func generateCode() (string, error) {
	b := make([]byte, codeLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeChars))))
		if err != nil {
			return "", err
		}
		b[i] = codeChars[n.Int64()]
	}
	return string(b), nil
}
```

- [ ] **Step 4: Run tests — confirm they pass**

```bash
cd signaling-server && go test ./internal/session/... -v
```

Expected:
```
--- PASS: TestCreate_ReturnsSessionWithEightCharCode
--- PASS: TestCreate_CodesAreUnique
--- PASS: TestGet_ReturnsExistingSession
--- PASS: TestGet_ReturnsFalseForExpired
--- PASS: TestGet_ReturnsFalseForUnknown
--- PASS: TestDelete_RemovesSession
ok  opencloudshare/server/internal/session
```

- [ ] **Step 5: Commit**

```bash
cd ..
git add signaling-server/internal/session/
git commit -m "feat(server): session manager with in-memory store and expiry"
```

---

## Task 3: Hub

**Files:**
- Create: `signaling-server/internal/hub/hub.go`

The hub tracks agent WebSocket connections by auth token and pairs them with browser connections by session ID. All message routing goes through hub methods — handlers never touch connections directly.

- [ ] **Step 1: Implement hub**

```go
// signaling-server/internal/hub/hub.go
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/coder/websocket"
)

type pair struct {
	agentConn   *websocket.Conn
	browserConn *websocket.Conn
}

type Hub struct {
	mu     sync.RWMutex
	agents map[string]*websocket.Conn // token → conn
	pairs  map[string]*pair           // sessionID → pair
}

func New() *Hub {
	return &Hub{
		agents: make(map[string]*websocket.Conn),
		pairs:  make(map[string]*pair),
	}
}

func (h *Hub) RegisterAgent(token string, conn *websocket.Conn) {
	h.mu.Lock()
	h.agents[token] = conn
	h.mu.Unlock()
}

func (h *Hub) UnregisterAgent(token string) {
	h.mu.Lock()
	delete(h.agents, token)
	h.mu.Unlock()
}

func (h *Hub) AgentConnected(token string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.agents[token]
	return ok
}

// PairSession associates a browser connection with the agent that owns sessionID.
// Returns an error if the agent is not currently connected.
func (h *Hub) PairSession(sessionID, agentToken string, browserConn *websocket.Conn) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	agentConn, ok := h.agents[agentToken]
	if !ok {
		return fmt.Errorf("agent not connected")
	}
	h.pairs[sessionID] = &pair{agentConn: agentConn, browserConn: browserConn}
	return nil
}

func (h *Hub) UnpairSession(sessionID string) {
	h.mu.Lock()
	delete(h.pairs, sessionID)
	h.mu.Unlock()
}

// SendToAgent sends a message to the agent identified by token.
func (h *Hub) SendToAgent(ctx context.Context, token string, msg any) error {
	h.mu.RLock()
	conn, ok := h.agents[token]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("agent not connected: %s", token)
	}
	return send(ctx, conn, msg)
}

// ForwardToAgent forwards a message to the agent in the paired session.
func (h *Hub) ForwardToAgent(ctx context.Context, sessionID string, msg any) error {
	h.mu.RLock()
	p, ok := h.pairs[sessionID]
	h.mu.RUnlock()
	if !ok {
		return nil // session already gone, ignore
	}
	return send(ctx, p.agentConn, msg)
}

// ForwardToBrowser forwards a message to the browser in the paired session.
func (h *Hub) ForwardToBrowser(ctx context.Context, sessionID string, msg any) error {
	h.mu.RLock()
	p, ok := h.pairs[sessionID]
	h.mu.RUnlock()
	if !ok {
		return nil // session already gone, ignore
	}
	return send(ctx, p.browserConn, msg)
}

func send(ctx context.Context, conn *websocket.Conn, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd signaling-server && go build ./internal/hub/...
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
cd ..
git add signaling-server/internal/hub/
git commit -m "feat(server): hub for agent/browser WebSocket connection routing"
```

---

## Task 4: Signaling server config + skeleton

**Files:**
- Create: `signaling-server/internal/config/config.go`
- Create: `signaling-server/cmd/server/main.go`

- [ ] **Step 1: Write config**

```go
// signaling-server/internal/config/config.go
package config

import "os"

type Config struct {
	Port      string
	AuthToken string
	STUNURL   string
}

func Load() *Config {
	return &Config{
		Port:      getEnv("PORT", "8080"),
		AuthToken: getEnv("AUTH_TOKEN", "dev-token"),
		STUNURL:   getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Write main.go skeleton**

```go
// signaling-server/cmd/server/main.go
package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/handler"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

func main() {
	cfg := config.Load()
	sessions := session.NewManager()
	h := hub.New()

	r := gin.Default()
	handler.RegisterRoutes(r, sessions, h, cfg)

	log.Printf("signaling server listening on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
```

- [ ] **Step 3: Create empty routes.go so it compiles**

```go
// signaling-server/internal/handler/routes.go
package handler

import (
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

func RegisterRoutes(r *gin.Engine, sessions *session.Manager, h *hub.Hub, cfg *config.Config) {
	// routes added in subsequent tasks
}
```

- [ ] **Step 4: Verify it compiles and starts**

```bash
cd signaling-server && go run ./cmd/server
```

Expected:
```
[GIN-debug] Listening and serving HTTP on :8080
```

Stop with Ctrl-C.

- [ ] **Step 5: Commit**

```bash
cd ..
git add signaling-server/internal/config/ signaling-server/cmd/ signaling-server/internal/handler/routes.go
git commit -m "feat(server): config loading and server skeleton"
```

---

## Task 5: REST handler — POST /api/v1/sessions

**Files:**
- Create: `signaling-server/internal/handler/rest.go`
- Modify: `signaling-server/internal/handler/routes.go`

- [ ] **Step 1: Write REST handler**

```go
// signaling-server/internal/handler/rest.go
package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

type createSessionRequest struct {
	ShareURL string `json:"share_url" binding:"required"`
	TTL      string `json:"ttl"`
}

type createSessionResponse struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expires_at"`
}

func CreateSession(sessions *session.Manager, h *hub.Hub, authToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c.GetHeader("Authorization"))
		if token != authToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		if !h.AgentConnected(token) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "agent not connected — connect WebSocket first"})
			return
		}

		var req createSessionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		ttl := 24 * time.Hour
		if req.TTL != "" {
			var err error
			ttl, err = time.ParseDuration(req.TTL)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ttl: use Go duration format e.g. 24h"})
				return
			}
		}

		s, err := sessions.Create(token, req.ShareURL, ttl)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
			return
		}

		c.JSON(http.StatusCreated, createSessionResponse{
			Code:      s.ID,
			ExpiresAt: s.ExpiresAt.Format(time.RFC3339),
		})
	}
}

func extractToken(authHeader string) string {
	return strings.TrimPrefix(authHeader, "Bearer ")
}
```

- [ ] **Step 2: Register route in routes.go**

```go
// signaling-server/internal/handler/routes.go
package handler

import (
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

func RegisterRoutes(r *gin.Engine, sessions *session.Manager, h *hub.Hub, cfg *config.Config) {
	r.POST("/api/v1/sessions", CreateSession(sessions, h, cfg.AuthToken))
}
```

- [ ] **Step 3: Compile check**

```bash
cd signaling-server && go build ./...
```

Expected: no output.

- [ ] **Step 4: Smoke test the REST endpoint**

Start the server in one terminal:
```bash
cd signaling-server && AUTH_TOKEN=dev-token go run ./cmd/server
```

In another terminal:
```bash
# Should fail — agent not connected yet
curl -s -X POST http://localhost:8080/api/v1/sessions \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"share_url":"https://example.com/s/TEST","ttl":"1h"}'
```

Expected: `{"error":"agent not connected — connect WebSocket first"}`

Stop the server.

- [ ] **Step 5: Commit**

```bash
cd ..
git add signaling-server/internal/handler/
git commit -m "feat(server): POST /api/v1/sessions REST handler"
```

---

## Task 6: Agent WebSocket handler

**Files:**
- Create: `signaling-server/internal/handler/agent_ws.go`
- Modify: `signaling-server/internal/handler/routes.go`

- [ ] **Step 1: Write agent WebSocket handler**

```go
// signaling-server/internal/handler/agent_ws.go
package handler

import (
	"encoding/json"
	"log"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/hub"
)

type agentMsg struct {
	Type      string          `json:"type"`
	Token     string          `json:"token,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
}

func AgentWS(h *hub.Hub, authToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // allow any origin in dev
		})
		if err != nil {
			log.Printf("agent_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx := c.Request.Context()
		var token string

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				if token != "" {
					log.Printf("agent disconnected: %s", token)
					h.UnregisterAgent(token)
				}
				return
			}

			var msg agentMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "register":
				if msg.Token != authToken {
					// token var is still "" here — use SendDirect, not SendToAgent
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "unauthorized"})
					conn.Close(websocket.StatusPolicyViolation, "unauthorized")
					return
				}
				token = msg.Token
				h.RegisterAgent(token, conn)
				log.Printf("agent registered: %s", token)
				h.SendToAgent(ctx, token, map[string]string{"type": "registered"})

			case "offer":
				if token == "" {
					continue
				}
				h.ForwardToBrowser(ctx, msg.SessionID, map[string]any{
					"type": "offer",
					"sdp":  msg.SDP,
				})

			case "ice_candidate":
				if token == "" {
					continue
				}
				h.ForwardToBrowser(ctx, msg.SessionID, map[string]any{
					"type":      "ice_candidate",
					"candidate": msg.Candidate,
				})
			}
		}
	}
}
```

- [ ] **Step 2: Register route**

```go
// signaling-server/internal/handler/routes.go
package handler

import (
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

func RegisterRoutes(r *gin.Engine, sessions *session.Manager, h *hub.Hub, cfg *config.Config) {
	r.POST("/api/v1/sessions", CreateSession(sessions, h, cfg.AuthToken))
	r.GET("/ws/agent", AgentWS(h, cfg.AuthToken))
}
```

- [ ] **Step 3: Compile check**

```bash
cd signaling-server && go build ./...
```

Expected: no output.

- [ ] **Step 4: Commit**

```bash
cd ..
git add signaling-server/internal/handler/agent_ws.go signaling-server/internal/handler/routes.go
git commit -m "feat(server): agent WebSocket handler — register + relay offer/ICE"
```

---

## Task 7: Browser WebSocket handler + web UI

**Files:**
- Create: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/internal/handler/routes.go`
- Create: `signaling-server/web/index.html`
- Create: `signaling-server/web/app.js`

- [ ] **Step 1: Write browser WebSocket handler**

```go
// signaling-server/internal/handler/browser_ws.go
package handler

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

type browserMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
}

func BrowserWS(h *hub.Hub, sessions *session.Manager, stunURL string) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Query("session")
		s, ok := sessions.Get(sessionID)
		if !ok {
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

		if err := h.PairSession(sessionID, s.Token, conn); err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		defer h.UnpairSession(sessionID)

		log.Printf("browser joined session %s", sessionID)

		// Send ICE config to browser
		hub.SendDirect(ctx, conn, map[string]any{
			"type": "ice_config",
			"ice_servers": []map[string]string{
				{"urls": stunURL},
			},
		})

		// Notify agent that browser has joined
		h.SendToAgent(ctx, s.Token, map[string]string{
			"type":       "join",
			"session_id": sessionID,
		})

		// Relay messages from browser to agent
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				log.Printf("browser disconnected from session %s", sessionID)
				return
			}

			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "answer":
				h.ForwardToAgent(ctx, sessionID, map[string]any{
					"type":       "answer",
					"session_id": sessionID,
					"sdp":        msg.SDP,
				})
			case "ice_candidate":
				h.ForwardToAgent(ctx, sessionID, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionID,
					"candidate":  msg.Candidate,
				})
			}
		}
	}
}
```

- [ ] **Step 2: Add SendDirect to hub (needed by browser_ws.go)**

Add this function to `signaling-server/internal/hub/hub.go`:

```go
// SendDirect sends a message directly to a connection without going through the hub index.
// Used for one-off messages before a connection is registered (e.g. error on join).
func SendDirect(ctx context.Context, conn *websocket.Conn, msg any) error {
	return send(ctx, conn, msg)
}
```

- [ ] **Step 3: Register browser WS route**

```go
// signaling-server/internal/handler/routes.go
package handler

import (
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

func RegisterRoutes(r *gin.Engine, sessions *session.Manager, h *hub.Hub, cfg *config.Config) {
	r.POST("/api/v1/sessions", CreateSession(sessions, h, cfg.AuthToken))
	r.GET("/ws/agent", AgentWS(h, cfg.AuthToken))
	r.GET("/ws/client", BrowserWS(h, sessions, cfg.STUNURL))
	r.StaticFile("/", "./web/index.html")
	r.StaticFile("/app.js", "./web/app.js")
}
```

- [ ] **Step 4: Write browser UI — index.html**

```html
<!-- signaling-server/web/index.html -->
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>OpenCloudShare</title>
  <style>
    body { font-family: sans-serif; max-width: 400px; margin: 80px auto; padding: 0 1rem; }
    input { width: 100%; padding: 8px; font-size: 1rem; box-sizing: border-box; }
    button { margin-top: 8px; padding: 8px 16px; font-size: 1rem; cursor: pointer; }
    #status { margin-top: 16px; font-weight: bold; }
  </style>
</head>
<body>
  <h2>OpenCloudShare</h2>
  <input id="code" placeholder="Enter session code" autocomplete="off" />
  <br>
  <button onclick="join()">Join</button>
  <div id="status"></div>
  <script src="/app.js"></script>
</body>
</html>
```

- [ ] **Step 5: Write browser UI — app.js**

```javascript
// signaling-server/web/app.js
let pc, ws;
let pendingCandidates = [];
let remoteDescSet = false;

function status(msg) {
  document.getElementById('status').textContent = msg;
}

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
            ws.send(JSON.stringify({
              type: 'ice_candidate',
              candidate: e.candidate.toJSON(),
            }));
          }
        };

        pc.ondatachannel = (e) => {
          e.channel.onopen = () => {
            status('✓ DataChannel open!');
            console.log('DataChannel open');
          };
        };

        pc.onconnectionstatechange = () => {
          if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
            status('Connection lost');
          }
        };
        break;

      case 'offer':
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        remoteDescSet = true;

        // Flush any candidates that arrived before the offer
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
  ws.onclose = () => { if (pc) pc.close(); };
}
```

- [ ] **Step 6: Compile check**

```bash
cd signaling-server && go build ./...
```

Expected: no output.

- [ ] **Step 7: Commit**

```bash
cd ..
git add signaling-server/internal/handler/ signaling-server/internal/hub/ signaling-server/web/
git commit -m "feat(server): browser WebSocket handler + web UI"
```

---

## Task 8: Agent config + signaling client

**Files:**
- Create: `agent/internal/config/config.go`
- Create: `agent/internal/signaling/client.go`

- [ ] **Step 1: Write agent config**

```go
// agent/internal/config/config.go
package config

import "os"

type Config struct {
	SignalingServer string // e.g. ws://localhost:8080
	AuthToken       string
	ShareURL        string
}

func Load() *Config {
	return &Config{
		SignalingServer: getEnv("SIGNALING_SERVER", "ws://localhost:8080"),
		AuthToken:       getEnv("AUTH_TOKEN", "dev-token"),
		ShareURL:        getEnv("SHARE_URL", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Write signaling client**

```go
// agent/internal/signaling/client.go
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
func (c *Client) CreateSession(ctx context.Context, shareURL, ttl string) (string, error) {
	body, _ := json.Marshal(map[string]string{"share_url": shareURL, "ttl": ttl})

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
		return err
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
```

- [ ] **Step 3: Compile check**

```bash
cd agent && go build ./internal/config/... && go build ./internal/signaling/...
```

Expected: no output.

- [ ] **Step 4: Commit**

```bash
cd ..
git add agent/internal/config/ agent/internal/signaling/
git commit -m "feat(agent): config loading and signaling server WebSocket client"
```

---

## Task 9: Agent peer (pion WebRTC)

**Files:**
- Create: `agent/internal/peer/peer.go`

- [ ] **Step 1: Write peer package**

```go
// agent/internal/peer/peer.go
package peer

import (
	"fmt"

	"github.com/pion/webrtc/v4"
)

// Peer manages a single WebRTC peer connection with a browser.
type Peer struct {
	pc             *webrtc.PeerConnection
	dc             *webrtc.DataChannel
	OnOpen         func()
	OnClosed       func()
	OnICECandidate func(init webrtc.ICECandidateInit)
}

// New creates a PeerConnection with the given ICE servers.
func New(iceServers []webrtc.ICEServer) (*Peer, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	p := &Peer{pc: pc}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // ICE gathering complete
		}
		if p.OnICECandidate != nil {
			p.OnICECandidate(c.ToJSON())
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if p.OnClosed != nil {
				p.OnClosed()
			}
		}
	})

	return p, nil
}

// CreateOffer creates a DataChannel, generates an SDP offer, and sets it as
// the local description. Returns the SDP string to send to the browser.
func (p *Peer) CreateOffer() (string, error) {
	dc, err := p.pc.CreateDataChannel("data", nil)
	if err != nil {
		return "", fmt.Errorf("create data channel: %w", err)
	}
	p.dc = dc

	dc.OnOpen(func() {
		if p.OnOpen != nil {
			p.OnOpen()
		}
	})

	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("create offer: %w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("set local description: %w", err)
	}
	return offer.SDP, nil
}

// SetAnswer applies the browser's SDP answer as the remote description.
func (p *Peer) SetAnswer(sdp string) error {
	return p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	})
}

// AddICECandidate adds an ICE candidate received from the browser.
func (p *Peer) AddICECandidate(init webrtc.ICECandidateInit) error {
	return p.pc.AddICECandidate(init)
}

// Close shuts down the peer connection.
func (p *Peer) Close() error {
	return p.pc.Close()
}
```

- [ ] **Step 2: Compile check**

```bash
cd agent && go build ./internal/peer/...
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
cd ..
git add agent/internal/peer/
git commit -m "feat(agent): pion WebRTC peer — offer, answer, ICE, DataChannel"
```

---

## Task 10: Agent main.go

**Files:**
- Create: `agent/cmd/agent/main.go`

- [ ] **Step 1: Write main.go**

```go
// agent/cmd/agent/main.go
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"

	"github.com/pion/webrtc/v4"
	"opencloudshare/agent/internal/config"
	"opencloudshare/agent/internal/peer"
	"opencloudshare/agent/internal/signaling"
)

func main() {
	cfg := config.Load()
	if cfg.ShareURL == "" {
		log.Fatal("SHARE_URL env var is required")
	}

	ctx := context.Background()

	sig := signaling.New(cfg.SignalingServer, cfg.AuthToken)
	if err := sig.Connect(ctx); err != nil {
		log.Fatalf("connect to signaling server: %v", err)
	}
	log.Printf("connected to signaling server at %s", cfg.SignalingServer)

	code, err := sig.CreateSession(ctx, cfg.ShareURL, "24h")
	if err != nil {
		log.Fatalf("create session: %v", err)
	}
	log.Printf("session ready — code: %s", code)
	log.Printf("open browser: http://localhost:8080  then enter code: %s", code)

	var (
		mu    sync.Mutex
		peers = make(map[string]*peer.Peer)
	)

	sig.OnMessage = func(msg signaling.Message) {
		switch msg.Type {
		case "registered":
			log.Println("agent registered with signaling server")

		case "join":
			log.Printf("browser joined session %s — starting WebRTC handshake", msg.SessionID)
			sessionID := msg.SessionID

			p, err := peer.New([]webrtc.ICEServer{
				{URLs: []string{"stun:stun.cloudflare.com:3478"}},
			})
			if err != nil {
				log.Printf("create peer: %v", err)
				return
			}

			mu.Lock()
			peers[sessionID] = p
			mu.Unlock()

			p.OnOpen = func() {
				log.Printf("✓ DataChannel open! (session %s)", sessionID)
			}
			p.OnClosed = func() {
				log.Printf("peer closed (session %s)", sessionID)
				mu.Lock()
				delete(peers, sessionID)
				mu.Unlock()
			}
			p.OnICECandidate = func(init webrtc.ICECandidateInit) {
				if err := sig.Send(ctx, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionID,
					"candidate":  init,
				}); err != nil {
					log.Printf("send ICE candidate: %v", err)
				}
			}

			sdp, err := p.CreateOffer()
			if err != nil {
				log.Printf("create offer: %v", err)
				return
			}
			if err := sig.Send(ctx, map[string]any{
				"type":       "offer",
				"session_id": sessionID,
				"sdp":        sdp,
			}); err != nil {
				log.Printf("send offer: %v", err)
			}

		case "answer":
			mu.Lock()
			p, ok := peers[msg.SessionID]
			mu.Unlock()
			if !ok {
				return
			}
			if err := p.SetAnswer(msg.SDP); err != nil {
				log.Printf("set answer: %v", err)
			}

		case "ice_candidate":
			mu.Lock()
			p, ok := peers[msg.SessionID]
			mu.Unlock()
			if !ok {
				return
			}
			var init webrtc.ICECandidateInit
			if err := json.Unmarshal(msg.Candidate, &init); err != nil {
				log.Printf("parse ICE candidate: %v", err)
				return
			}
			if err := p.AddICECandidate(init); err != nil {
				log.Printf("add ICE candidate: %v", err)
			}

		case "error":
			log.Printf("signaling error: %s", msg.Err)
		}
	}

	log.Println("waiting for browser connections (Ctrl-C to stop)...")
	if err := sig.Listen(ctx); err != nil {
		log.Printf("signaling disconnected: %v", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Compile check**

```bash
cd agent && go build ./...
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
cd ..
git add agent/cmd/
git commit -m "feat(agent): main — connect, create session, handle WebRTC signaling"
```

---

## Task 11: End-to-end smoke test

This is a manual test. Verify that a DataChannel opens between the agent and a browser.

- [ ] **Step 1: Start the signaling server**

Open a terminal:
```bash
cd /Users/ali/Git/OpenCloudShare/signaling-server
AUTH_TOKEN=dev-token go run ./cmd/server
```

Expected:
```
[GIN-debug] Listening and serving HTTP on :8080
```

- [ ] **Step 2: Start the agent**

Open a second terminal:
```bash
cd /Users/ali/Git/OpenCloudShare/agent
AUTH_TOKEN=dev-token \
SHARE_URL=https://cloud.afrino-ratio.ts.net/s/NtYwGbSLxITQxgY \
go run ./cmd/agent
```

Expected (within a few seconds):
```
connected to signaling server at ws://localhost:8080
agent registered with signaling server
session ready — code: <8-char-code>
open browser: http://localhost:8080  then enter code: <8-char-code>
waiting for browser connections (Ctrl-C to stop)...
```

- [ ] **Step 3: Open the browser and join**

1. Open `http://localhost:8080` in Chrome or Firefox
2. Enter the 8-character code printed by the agent
3. Click **Join**

Expected — browser shows: `✓ DataChannel open!`

Expected — agent terminal logs:
```
browser joined session <code> — starting WebRTC handshake
✓ DataChannel open! (session <code>)
```

- [ ] **Step 4: Confirm ICE used local candidates (localhost)**

Open the browser DevTools console. You should see no ICE errors. On localhost, the connection will use `host` candidates (no STUN needed) — this is expected and fast.

- [ ] **Step 5: Commit**

```bash
cd /Users/ali/Git/OpenCloudShare
git add .
git commit -m "feat: Slice 1 complete — WebRTC DataChannel open end-to-end"
```

---

## Summary

After completing all tasks, the system can:
- Accept agent WebSocket connections and session creation via REST
- Accept browser connections by session code
- Relay SDP offer/answer and ICE candidates between agent and browser
- Open a WebRTC DataChannel (logged on both sides)

**Next: Slice 2 — File Transfer** (OpenCloud WebDAV listing + DataChannel chunked streaming)
