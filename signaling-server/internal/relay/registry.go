package relay

import (
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"
)

// AgentRegistry maps API key IDs ↔ agent libp2p peer IDs.
// Register is called by the agent connection handler (Slice 13b).
type AgentRegistry struct {
	mu     sync.RWMutex
	byKey  map[string]peer.ID
	byPeer map[peer.ID]string
}

func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{byKey: make(map[string]peer.ID), byPeer: make(map[peer.ID]string)}
}

func (r *AgentRegistry) Register(apiKeyID string, pid peer.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Clean up old peer association for this key
	if oldPid, exists := r.byKey[apiKeyID]; exists {
		delete(r.byPeer, oldPid)
	}
	// Clean up old key association for this peer
	if oldKey, exists := r.byPeer[pid]; exists {
		delete(r.byKey, oldKey)
	}

	r.byKey[apiKeyID] = pid
	r.byPeer[pid] = apiKeyID
}

func (r *AgentRegistry) Unregister(apiKeyID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pid, ok := r.byKey[apiKeyID]; ok {
		delete(r.byPeer, pid)
	}
	delete(r.byKey, apiKeyID)
}

func (r *AgentRegistry) Lookup(apiKeyID string) (peer.ID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pid, ok := r.byKey[apiKeyID]
	return pid, ok
}

func (r *AgentRegistry) Reverse(pid peer.ID) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.byPeer[pid]
	return k, ok
}