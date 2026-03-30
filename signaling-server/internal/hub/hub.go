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

// SendDirect sends a message directly to a connection without going through the hub index.
// Used for one-off messages before a connection is registered (e.g. error on join).
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
