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
	apiKeyID    string
	agentConn   *websocket.Conn
	browserConn *websocket.Conn
}

type browserOwner struct {
	apiKeyID  string
	sessionID string
	agentConn *websocket.Conn
}

type Hub struct {
	mu              sync.RWMutex
	agents          map[string]*websocket.Conn // apiKey → conn (one conn per API key)
	codes           map[string]string          // code → apiKey (for browser lookup)
	pairs           map[string]*pair           // sessionID → pair
	connBrowsers    map[string]*websocket.Conn // connID → browser conn
	browserOwners   map[string]browserOwner
	connFails       map[string]int // connID → auth failure count
	connImmichFails map[string]int // connID → Immich password auth failure count
	connWrites      map[*websocket.Conn]*sync.Mutex
}

func New() *Hub {
	return &Hub{
		agents:          make(map[string]*websocket.Conn),
		codes:           make(map[string]string),
		pairs:           make(map[string]*pair),
		connBrowsers:    make(map[string]*websocket.Conn),
		browserOwners:   make(map[string]browserOwner),
		connFails:       make(map[string]int),
		connImmichFails: make(map[string]int),
		connWrites:      make(map[*websocket.Conn]*sync.Mutex),
	}
}

func (h *Hub) RegisterAgent(apiKey string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[apiKey] = conn
	// Rebind still-owned browsers atomically so reconnecting the agent does not
	// strand an existing browser's knock/join flow. Replace pair snapshots rather
	// than mutating pointers already held by a forwarding goroutine.
	for code, sessionPair := range h.pairs {
		if sessionPair.apiKeyID == apiKey && h.codes[code] == apiKey {
			h.pairs[code] = &pair{apiKeyID: apiKey, agentConn: conn, browserConn: sessionPair.browserConn}
		}
	}
	for connID, owner := range h.browserOwners {
		if owner.apiKeyID == apiKey && h.codes[owner.sessionID] == apiKey {
			owner.agentConn = conn
			h.browserOwners[connID] = owner
		}
	}
	h.ensureWriteMuLocked(conn)
}

func (h *Hub) UnregisterAgent(apiKey string, expected *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if expected == nil {
		return
	}
	delete(h.connWrites, expected)
	if h.agents[apiKey] != expected {
		return
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

func (h *Hub) UnregisterCode(ctx context.Context, code, apiKey, reason string) {
	h.mu.Lock()
	owner, ok := h.codes[code]
	if !ok || owner != apiKey {
		h.mu.Unlock()
		return
	}
	delete(h.codes, code)
	sessionPair := h.pairs[code]
	delete(h.pairs, code)
	h.mu.Unlock()

	if sessionPair != nil && sessionPair.browserConn != nil {
		send(ctx, h, sessionPair.browserConn, map[string]string{"type": "error", "message": reason})
		sessionPair.browserConn.CloseNow()
	}
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

// PairSession atomically pairs and registers a browser with its persisted share owner.
func (h *Hub) PairSession(sessionID, connID, apiKeyID string, browserConn *websocket.Conn) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Direct lookup - don't call GetAgentConn which also acquires the lock
	apiKey, ok := h.codes[sessionID]
	if !ok || apiKey != apiKeyID {
		return fmt.Errorf("code not registered")
	}
	agentConn, ok := h.agents[apiKey]
	if !ok {
		return fmt.Errorf("agent not connected")
	}
	if connID == "" || browserConn == nil {
		return fmt.Errorf("browser identity required")
	}
	h.pairs[sessionID] = &pair{apiKeyID: apiKey, agentConn: agentConn, browserConn: browserConn}
	h.connBrowsers[connID] = browserConn
	h.browserOwners[connID] = browserOwner{apiKeyID: apiKey, sessionID: sessionID, agentConn: agentConn}
	h.ensureWriteMuLocked(browserConn)
	return nil
}

// UnpairSession removes the active browser pair for sessionID, but only if the
// caller is cleaning up the browser connection that is still current.
func (h *Hub) UnpairSession(sessionID string, browserConn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessionPair, ok := h.pairs[sessionID]
	if !ok {
		return
	}
	if browserConn != nil && sessionPair.browserConn != browserConn {
		return
	}
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

func (h *Hub) ForwardToBrowser(ctx context.Context, apiKeyID string, agentConn *websocket.Conn, sessionID string, msg any) error {
	h.mu.RLock()
	sessionPair, ok := h.pairs[sessionID]
	authorized := ok && h.agentOwnsSessionLocked(apiKeyID, agentConn, sessionID) && sessionPair.agentConn == agentConn
	h.mu.RUnlock()
	if !authorized {
		return nil
	}
	return send(ctx, h, sessionPair.browserConn, msg)
}

func (h *Hub) SendDirect(ctx context.Context, conn *websocket.Conn, msg any) error {
	return send(ctx, h, conn, msg)
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

// UnregisterBrowserConn removes the browser conn and its failure count.
// Called in the browser_ws.go defer when the WebSocket closes.
func (h *Hub) UnregisterBrowserConn(connID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn, ok := h.connBrowsers[connID]; ok {
		delete(h.connWrites, conn)
	}
	delete(h.connBrowsers, connID)
	delete(h.browserOwners, connID)
	delete(h.connFails, connID)
	delete(h.connImmichFails, connID)
}

// ForwardToBrowserByConnID sends only when the current agent owns the browser binding.
// Used to route nonce responses from the agent back to the correct browser.
func (h *Hub) ForwardToBrowserByConnID(ctx context.Context, apiKeyID string, agentConn *websocket.Conn, connID string, msg any) error {
	h.mu.RLock()
	conn, _, ok := h.browserForAgentLocked(apiKeyID, agentConn, connID)
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	return send(ctx, h, conn, msg)
}

// IncrementAuthFailure increments the failure count for connID and returns
// the new total, or zero for an unauthorized target.
func (h *Hub) IncrementAuthFailure(apiKeyID string, agentConn *websocket.Conn, connID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, _, ok := h.browserForAgentLocked(apiKeyID, agentConn, connID); !ok {
		return 0
	}
	h.connFails[connID]++
	return h.connFails[connID]
}

// IncrementImmichAuthFailure increments the Immich password failure count for
// connID and returns the new total, or zero for an unauthorized target.
func (h *Hub) IncrementImmichAuthFailure(apiKeyID string, agentConn *websocket.Conn, connID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, _, ok := h.browserForAgentLocked(apiKeyID, agentConn, connID); !ok {
		return 0
	}
	h.connImmichFails[connID]++
	return h.connImmichFails[connID]
}

// CloseBrowserConnWithError sends an error message to the browser identified by
// connID and then closes its WebSocket.
func (h *Hub) CloseBrowserConnWithError(ctx context.Context, apiKeyID string, agentConn *websocket.Conn, connID, message string) {
	h.mu.RLock()
	conn, _, ok := h.browserForAgentLocked(apiKeyID, agentConn, connID)
	h.mu.RUnlock()
	if !ok {
		return
	}
	send(ctx, h, conn, map[string]string{"type": "error", "message": message})
	conn.Close(websocket.StatusNormalClosure, message)
}
