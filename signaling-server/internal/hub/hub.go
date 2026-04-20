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
	mu           sync.RWMutex
	agents       map[string]*websocket.Conn // apiKey → conn (one conn per API key)
	codes        map[string]string          // code → apiKey (for browser lookup)
	pairs        map[string]*pair           // sessionID → pair
	connBrowsers map[string]*websocket.Conn // connID → browser conn
	connFails    map[string]int             // connID → auth failure count
	connWrites   map[*websocket.Conn]*sync.Mutex
}

func New() *Hub {
	return &Hub{
		agents:       make(map[string]*websocket.Conn),
		codes:        make(map[string]string),
		pairs:        make(map[string]*pair),
		connBrowsers: make(map[string]*websocket.Conn),
		connFails:    make(map[string]int),
		connWrites:   make(map[*websocket.Conn]*sync.Mutex),
	}
}

func (h *Hub) RegisterAgent(apiKey string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[apiKey] = conn
	h.ensureWriteMuLocked(conn)
}

func (h *Hub) UnregisterAgent(apiKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn, ok := h.agents[apiKey]; ok {
		delete(h.connWrites, conn)
	}
	delete(h.agents, apiKey)
}

func (h *Hub) AgentConnected(apiKey string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.agents[apiKey]
	return ok
}

func (h *Hub) RegisterCode(code, apiKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.codes[code] = apiKey
}

func (h *Hub) GetAgentConn(code string) (*websocket.Conn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	apiKey, ok := h.codes[code]
	if !ok {
		return nil, false
	}
	conn, ok := h.agents[apiKey]
	return conn, ok
}

// PairSession associates a browser connection with the agent that owns sessionID.
func (h *Hub) PairSession(sessionID string, browserConn *websocket.Conn) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Direct lookup - don't call GetAgentConn which also acquires the lock
	apiKey, ok := h.codes[sessionID]
	if !ok {
		return fmt.Errorf("code not registered")
	}
	agentConn, ok := h.agents[apiKey]
	if !ok {
		return fmt.Errorf("agent not connected")
	}
	h.pairs[sessionID] = &pair{agentConn: agentConn, browserConn: browserConn}
	return nil
}

func (h *Hub) UnpairSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pairs, sessionID)
}

func (h *Hub) SendToAgent(ctx context.Context, apiKey string, msg any) error {
	h.mu.RLock()
	conn, ok := h.agents[apiKey]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("agent not connected: %s", apiKey)
	}
	return send(ctx, h, conn, msg)
}

func (h *Hub) ForwardToAgent(ctx context.Context, sessionID string, msg any) error {
	h.mu.RLock()
	sessionPair, ok := h.pairs[sessionID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	return send(ctx, h, sessionPair.agentConn, msg)
}

func (h *Hub) ForwardToBrowser(ctx context.Context, sessionID string, msg any) error {
	h.mu.RLock()
	sessionPair, ok := h.pairs[sessionID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	return send(ctx, h, sessionPair.browserConn, msg)
}

func SendDirect(ctx context.Context, conn *websocket.Conn, msg any) error {
	return send(ctx, nil, conn, msg)
}

func send(ctx context.Context, h *Hub, conn *websocket.Conn, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if h != nil {
		mu := h.writeMu(conn)
		mu.Lock()
		defer mu.Unlock()
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func (h *Hub) ensureWriteMuLocked(conn *websocket.Conn) {
	if conn == nil {
		return
	}
	if _, ok := h.connWrites[conn]; !ok {
		h.connWrites[conn] = &sync.Mutex{}
	}
}

func (h *Hub) writeMu(conn *websocket.Conn) *sync.Mutex {
	if conn == nil {
		return &sync.Mutex{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureWriteMuLocked(conn)
	return h.connWrites[conn]
}

// CloseAgent closes the WebSocket connection for the agent identified by apiKeyID,
// removes it from the hub, and purges all associated codes and pairs.
// Used when an API key is revoked.
func (h *Hub) CloseAgent(apiKeyID string) {
	h.mu.Lock()
	conn, ok := h.agents[apiKeyID]
	if ok {
		delete(h.agents, apiKeyID)
	}

	// Purge all codes that belonged to this agent.
	var staleCodes []string
	for code, keyID := range h.codes {
		if keyID == apiKeyID {
			staleCodes = append(staleCodes, code)
			delete(h.codes, code)
		}
	}

	// Collect and remove any active pairs for the stale codes, grabbing browser
	// conns so we can close them outside the lock.
	var browserConns []*websocket.Conn
	for _, code := range staleCodes {
		if sessionPair, exists := h.pairs[code]; exists {
			if sessionPair.browserConn != nil {
				browserConns = append(browserConns, sessionPair.browserConn)
			}
			delete(h.pairs, code)
		}
	}
	h.mu.Unlock()

	if ok && conn != nil {
		h.mu.Lock()
		delete(h.connWrites, conn)
		h.mu.Unlock()
		conn.Close(websocket.StatusPolicyViolation, "API key revoked")
	}
	for _, browserConn := range browserConns {
		h.mu.Lock()
		delete(h.connWrites, browserConn)
		h.mu.Unlock()
		browserConn.Close(websocket.StatusNormalClosure, "agent disconnected")
	}
}

// RegisterBrowserConn stores a browser WebSocket connection keyed by connID.
// Called when a browser WebSocket connects, before the knock/join flow.
func (h *Hub) RegisterBrowserConn(connID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connBrowsers[connID] = conn
	h.ensureWriteMuLocked(conn)
}

// UnregisterBrowserConn removes the browser conn and its failure count.
// Called in the browser_ws.go defer when the WebSocket closes.
func (h *Hub) UnregisterBrowserConn(connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn, ok := h.connBrowsers[connID]; ok {
		delete(h.connWrites, conn)
	}
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
	return send(ctx, h, conn, msg)
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
	send(ctx, h, conn, map[string]string{"type": "error", "message": message})
	conn.Close(websocket.StatusNormalClosure, message)
}
