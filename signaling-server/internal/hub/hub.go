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
	agents map[string]*websocket.Conn // apiKey → conn (one conn per API key)
	codes  map[string]string          // code → apiKey (for browser lookup)
	pairs  map[string]*pair           // sessionID → pair
}

func New() *Hub {
	return &Hub{
		agents: make(map[string]*websocket.Conn),
		codes:  make(map[string]string),
		pairs:  make(map[string]*pair),
	}
}

func (h *Hub) RegisterAgent(apiKey string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[apiKey] = conn
}

func (h *Hub) UnregisterAgent(apiKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
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
	return send(ctx, conn, msg)
}

func (h *Hub) ForwardToAgent(ctx context.Context, sessionID string, msg any) error {
	h.mu.RLock()
	p, ok := h.pairs[sessionID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	return send(ctx, p.agentConn, msg)
}

func (h *Hub) ForwardToBrowser(ctx context.Context, sessionID string, msg any) error {
	h.mu.RLock()
	p, ok := h.pairs[sessionID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	return send(ctx, p.browserConn, msg)
}

func SendDirect(ctx context.Context, conn *websocket.Conn, msg any) error {
	return send(ctx, conn, msg)
}

func send(ctx context.Context, conn *websocket.Conn, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
