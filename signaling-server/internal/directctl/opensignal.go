package directctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NOTE: `OpenAck` and `openWaiter` are ALREADY defined in controller.go (Task 10,
// from the Contracts section). Do NOT re-declare them here — import and use them.

// EmitOpen sends an open_signal to the agent for shareID and blocks until the
// agent's open_ack (correlated on the full tuple) arrives, the context is
// cancelled, or the ack timeout elapses.
func (c *Controller) EmitOpen(ctx context.Context, apiKeyID, shareID, origin string, lease time.Duration) (OpenAck, error) {
	nonce := newNonce()
	c.seqMu.Lock()
	c.seq[apiKeyID]++
	seq := c.seq[apiKeyID]
	c.seqMu.Unlock()

	agentID := c.epochAgentID(apiKeyID)

	sig := map[string]any{
		"type": "open_signal", "version": 1, "agent_id": agentID,
		"share_id": shareID, "route": "direct", "nonce": nonce, "seq": seq,
		"lease_seconds": int(lease.Seconds()),
		"expires_at":    time.Now().Add(2 * time.Minute).Format(time.RFC3339),
	}

	w := &openWaiter{apiKeyID: apiKeyID, shareID: shareID, seq: seq, ch: make(chan OpenAck, 1)}
	c.waiterMu.Lock()
	c.waiters[nonce] = w
	c.waiterMu.Unlock()
	defer func() {
		c.waiterMu.Lock()
		if c.waiters[nonce] == w {
			delete(c.waiters, nonce)
		}
		c.waiterMu.Unlock()
	}()

	if err := c.sendToAgentFn(ctx, apiKeyID, sig); err != nil {
		return OpenAck{}, fmt.Errorf("send open_signal: %w", err)
	}

	timeout := c.ackTimeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	select {
	case ack := <-w.ch:
		return ack, nil
	case <-time.After(timeout):
		return OpenAck{}, fmt.Errorf("open_ack timeout")
	case <-ctx.Done():
		return OpenAck{}, ctx.Err()
	}
}

// HandleOpenAck resolves a pending open_signal waiter. The ack is only
// accepted when the FULL tuple (apiKeyID + nonce + share_id + seq) matches the
// waiter; any mismatch discards the ack without completing the waiter. On a
// match the waiter is removed from the map (deferred cleanup in EmitOpen makes
// a late ack after timeout a no-op).
func (c *Controller) HandleOpenAck(apiKeyID string, ack OpenAck) {
	c.waiterMu.Lock()
	w, ok := c.waiters[ack.Nonce]
	if ok && (w.apiKeyID != apiKeyID || w.shareID != ack.ShareID || w.seq != ack.Seq) {
		ok = false // full-tuple mismatch → discard
	}
	if ok {
		delete(c.waiters, ack.Nonce)
	}
	c.waiterMu.Unlock()
	if ok {
		select {
		case w.ch <- ack:
		default:
		}
	}
}

// epochAgentID returns the hello-supplied agent_id for the current connection
// epoch, or "" if no epoch is installed. open_signal.agent_id must be this
// stored hello agent_id (what the agent's SignalGate is constructed with),
// never the apiKeyID.
func (c *Controller) epochAgentID(apiKeyID string) string {
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	if e := c.epochs[apiKeyID]; e != nil {
		return e.agentID
	}
	return ""
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
