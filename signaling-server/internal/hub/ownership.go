package hub

import "github.com/coder/websocket"

// IsCurrentAgent fences messages from sockets superseded by a reconnect.
func (h *Hub) IsCurrentAgent(apiKeyID string, conn *websocket.Conn) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return apiKeyID != "" && conn != nil && h.agents[apiKeyID] == conn
}

// BrowserSessionForAgent resolves an owned browser binding, never a caller-supplied share code.
func (h *Hub) BrowserSessionForAgent(apiKeyID string, conn *websocket.Conn, connID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, code, ok := h.browserForAgentLocked(apiKeyID, conn, connID)
	return code, ok
}

// Callers hold h.mu for the authorization check and destination snapshot.
func (h *Hub) agentOwnsSessionLocked(apiKeyID string, conn *websocket.Conn, code string) bool {
	return apiKeyID != "" && conn != nil && h.agents[apiKeyID] == conn && h.codes[code] == apiKeyID
}

func (h *Hub) browserForAgentLocked(apiKeyID string, conn *websocket.Conn, connID string) (*websocket.Conn, string, bool) {
	owner, ok := h.browserOwners[connID]
	browser := h.connBrowsers[connID]
	if !ok || browser == nil || owner.apiKeyID != apiKeyID || owner.agentConn != conn || !h.agentOwnsSessionLocked(apiKeyID, conn, owner.sessionID) {
		return nil, "", false
	}
	return browser, owner.sessionID, true
}
