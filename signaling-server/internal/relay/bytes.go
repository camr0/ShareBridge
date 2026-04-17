package relay

import (
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	pbv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/pb"
	"github.com/multiformats/go-multiaddr"
)

// ByteTracker implements both circuitv2.ACLFilter and circuitv2.MetricsTracer.
// It tracks per-browser-peer bytes transferred through the relay, since the
// standard BandwidthCounter doesn't track relayed traffic.
type ByteTracker struct {
	mu sync.Mutex

	// aclEntries maps browser peer ID to ACL entry (authorization info)
	aclEntries map[peer.ID]*circuitEntry

	// activeBrowser is the browser peer ID that currently has an open circuit.
	// Only one circuit per browser at a time (browser is single-threaded).
	activeBrowser peer.ID

	// byteCounts maps browser peer ID to cumulative bytes transferred.
	// Bytes are attributed to the active browser when BytesTransferred is called.
	byteCounts map[peer.ID]int64

	// ttl is the ACL entry expiration duration.
	ttl time.Duration

	// connectionsOpened counts total circuit connections for MetricsTracer.
	connectionsOpened int
}

// NewByteTracker creates a new byte tracker with the given TTL.
func NewByteTracker(ttl time.Duration) *ByteTracker {
	return &ByteTracker{
		aclEntries:  make(map[peer.ID]*circuitEntry),
		byteCounts:  make(map[peer.ID]int64),
		ttl:         ttl,
	}
}

// --- ACLFilter interface ---

// AllowReserve permits any peer to make a circuit relay reservation.
func (bt *ByteTracker) AllowReserve(_ peer.ID, _ multiaddr.Multiaddr) bool {
	return true
}

// AllowConnect permits src (browser) to open a circuit to dst (agent) only if authorized.
// It also records src as the active browser for byte attribution.
func (bt *ByteTracker) AllowConnect(src peer.ID, _ multiaddr.Multiaddr, dst peer.ID) bool {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	entry, ok := bt.aclEntries[src]
	if !ok {
		return false
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(bt.aclEntries, src)
		return false
	}

	// Record this browser as active for byte attribution
	bt.activeBrowser = src

	return entry.AgentPeerID == dst
}

// Authorize records that browserPeerID may open a circuit to agentPeerID for ttl duration.
func (bt *ByteTracker) Authorize(browserPeerID, agentPeerID peer.ID, shareCode, apiKeyID string, ttl time.Duration) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	bt.aclEntries[browserPeerID] = &circuitEntry{
		AgentPeerID: agentPeerID,
		ShareCode:   shareCode,
		APIKeyID:    apiKeyID,
		ExpiresAt:   time.Now().Add(ttl),
	}
}

// GetEntry returns the ACL entry for a browser peer ID.
func (bt *ByteTracker) GetEntry(browserPeerID peer.ID) (*circuitEntry, bool) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	entry, ok := bt.aclEntries[browserPeerID]
	return entry, ok
}

// Remove deletes the ACL entry for a browser peer ID.
func (bt *ByteTracker) Remove(browserPeerID peer.ID) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	delete(bt.aclEntries, browserPeerID)
}

// GetBytes returns the cumulative bytes transferred for a browser peer ID.
func (bt *ByteTracker) GetBytes(browserPeerID peer.ID) int64 {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	return bt.byteCounts[browserPeerID]
}

// --- MetricsTracer interface ---

// RelayStatus tracks whether the relay service is active.
func (bt *ByteTracker) RelayStatus(enabled bool) {
	// No-op for our tracking purposes
}

// ConnectionOpened tracks a circuit connection being opened.
func (bt *ByteTracker) ConnectionOpened() {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	bt.connectionsOpened++
}

// ConnectionClosed tracks a circuit connection being closed.
func (bt *ByteTracker) ConnectionClosed(d time.Duration) {
	// Connection closed - clear active browser
	bt.mu.Lock()
	bt.activeBrowser = ""
	bt.mu.Unlock()
}

// ConnectionRequestHandled tracks a connection request status.
func (bt *ByteTracker) ConnectionRequestHandled(status pbv2.Status) {
	// No-op for our tracking
}

// ReservationAllowed tracks a reservation being allowed.
func (bt *ByteTracker) ReservationAllowed(isRenewal bool) {
	// No-op for our tracking
}

// ReservationClosed tracks a reservation being closed.
func (bt *ByteTracker) ReservationClosed(cnt int) {
	// No-op for our tracking
}

// ReservationRequestHandled tracks a reservation request status.
func (bt *ByteTracker) ReservationRequestHandled(status pbv2.Status) {
	// No-op for our tracking
}

// BytesTransferred tracks bytes relayed. We attribute these to the active browser.
func (bt *ByteTracker) BytesTransferred(cnt int) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if bt.activeBrowser != "" {
		bt.byteCounts[bt.activeBrowser] += int64(cnt)
	}
}