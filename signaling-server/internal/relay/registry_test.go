package relay_test

import (
	"testing"
	"time"

	"github.com/coder/websocket"
	"sharebridge/server/internal/relay"
)

func TestRegistry_BrowserWaitsForAgentWithinPendingWindow(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-1",
		AccountID:    "acct-1",
		SessionCode:  "SHARE123",
		RelayAllowed: true,
		JTI:          "jti-1",
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}

	browserStatePeer, browserState, err := reg.BindBrowserSocket("sid-1", "jti-1", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if browserStatePeer != nil {
		t.Error("expected nil peer, browser is first")
	}
	if browserState != relay.StatePendingBrowser {
		t.Errorf("expected StatePendingBrowser, got %v", browserState)
	}

	// Wait goroutine
	waitDone := make(chan *websocket.Conn, 1)
	go func() {
		peer, waitErr := reg.WaitForAgent("sid-1", now)
		if waitErr != nil {
			t.Error(waitErr)
		}
		waitDone <- peer
	}()

	agentStatePeer, agentState, err := reg.BindAgentSocket("sid-1", "agent-1", nil, now.Add(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if agentStatePeer != nil {
		t.Error("expected nil peer for agent")
	}
	if agentState != relay.StateActive {
		t.Errorf("expected StateActive, got %v", agentState)
	}
	if peer := <-waitDone; peer != nil {
		t.Error("expected nil peer from wait")
	}
}

func TestRegistry_AgentWaitsForBrowserWithinPendingWindow(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-2",
		AccountID:    "acct-1",
		SessionCode:  "SHARE456",
		AgentID:      "agent-1",
		RelayAllowed: true,
		JTI:          "jti-2",
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}

	agentStatePeer, agentState, err := reg.BindAgentSocket("sid-2", "agent-1", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if agentStatePeer != nil {
		t.Error("expected nil peer for agent")
	}
	if agentState != relay.StatePendingAgent {
		t.Errorf("expected StatePendingAgent, got %v", agentState)
	}

	// Wait goroutine
	waitDone := make(chan *websocket.Conn, 1)
	go func() {
		peer, waitErr := reg.WaitForBrowser("sid-2", now)
		if waitErr != nil {
			t.Error(waitErr)
		}
		waitDone <- peer
	}()

	browserStatePeer, browserState, err := reg.BindBrowserSocket("sid-2", "jti-2", nil, now.Add(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if browserStatePeer != nil {
		t.Error("expected nil peer for browser")
	}
	if browserState != relay.StateActive {
		t.Errorf("expected StateActive, got %v", browserState)
	}
	if peer := <-waitDone; peer != nil {
		t.Error("expected nil peer from wait")
	}
}

func TestRegistry_RejectsSpentJTIReplay(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-1",
		JTI:          "jti-1",
		RelayAllowed: true,
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}

	_, _, err := reg.BindBrowserSocket("sid-1", "jti-1", nil, now)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = reg.BindBrowserSocket("sid-1", "jti-1", nil, now.Add(100*time.Millisecond))
	if err == nil {
		t.Error("expected error for replayed JTI")
	}
	if err != relay.ErrReplay {
		t.Errorf("expected ErrReplay, got %v", err)
	}
}

func TestRegistry_CleanupDirectSuccessRemovesPendingSession(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-1",
		JTI:          "jti-1",
		RelayAllowed: true,
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}
	reg.MarkDirectSuccess("sid-1")

	_, err := reg.Get("sid-1")
	if err == nil {
		t.Error("expected error for removed session")
	}
	if err != relay.ErrUnknownSID {
		t.Errorf("expected ErrUnknownSID, got %v", err)
	}
}