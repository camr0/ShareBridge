package relay

import (
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// circuitEntry holds the state for a JWT-authorized browser → agent pair.
type circuitEntry struct {
	AgentPeerID peer.ID
	ShareCode   string
	APIKeyID    string
	ExpiresAt   time.Time
}

// CircuitACL implements circuitv2.ACLFilter.
// Agents may always reserve. Browsers may only dial the specific agent named in their JWT.
type CircuitACL struct {
	mu      sync.Mutex
	entries map[peer.ID]*circuitEntry // browserPeerID → entry
}

func newCircuitACL(_ time.Duration) *CircuitACL {
	return &CircuitACL{entries: make(map[peer.ID]*circuitEntry)}
}

// AllowReserve permits any peer to make a circuit relay reservation.
// Agent authentication happens via a separate API-key protocol (Slice 13b).
func (a *CircuitACL) AllowReserve(_ peer.ID, _ multiaddr.Multiaddr) bool {
	return true
}

// AllowConnect permits src to open a circuit to dst only if Authorize was called
// for (src, dst) with a non-expired TTL.
func (a *CircuitACL) AllowConnect(src peer.ID, _ multiaddr.Multiaddr, dst peer.ID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[src]
	if !ok {
		return false
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(a.entries, src)
		return false
	}
	return entry.AgentPeerID == dst
}

// Authorize records that browserPeerID may open a circuit to agentPeerID for ttl duration.
// Called by the /sharebridge/relay/1.0.0 auth handler after JWT validation.
func (a *CircuitACL) Authorize(browserPeerID, agentPeerID peer.ID, shareCode, apiKeyID string, ttl time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries[browserPeerID] = &circuitEntry{
		AgentPeerID: agentPeerID,
		ShareCode:   shareCode,
		APIKeyID:    apiKeyID,
		ExpiresAt:   time.Now().Add(ttl),
	}
}

// GetEntry returns the entry for a browser peer ID (used by the bandwidth notifier).
func (a *CircuitACL) GetEntry(browserPeerID peer.ID) (*circuitEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[browserPeerID]
	return entry, ok
}

// Remove deletes the entry for a browser peer ID (called after byte accounting at disconnect).
func (a *CircuitACL) Remove(browserPeerID peer.ID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.entries, browserPeerID)
}