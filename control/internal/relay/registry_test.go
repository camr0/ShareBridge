package relay_test

import (
	"testing"
	"time"

	"github.com/coder/websocket"
	"sharebridge/control/internal/relay"
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

func TestRegistry_CloseSessionReturnsValues(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-1",
		AccountID:    "acct-42",
		JTI:          "jti-1",
		RelayAllowed: true,
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}

	// Add some forwarded bytes
	reg.AddForwardedBytes("sid-1", 100)
	reg.AddForwardedBytes("sid-1", 250)

	accountID, bytes, err := reg.CloseSession("sid-1")
	if err != nil {
		t.Fatal(err)
	}
	if accountID != "acct-42" {
		t.Errorf("expected accountID 'acct-42', got %q", accountID)
	}
	if bytes != 350 {
		t.Errorf("expected bytes 350, got %d", bytes)
	}

	// Session should be removed
	_, err = reg.Get("sid-1")
	if err != relay.ErrUnknownSID {
		t.Errorf("expected ErrUnknownSID after close, got %v", err)
	}
}

func TestRegistry_CloseSessionCleansUpSpentJTI(t *testing.T) {
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

	// Bind browser socket to mark JTI as spent
	_, _, err := reg.BindBrowserSocket("sid-1", "jti-1", nil, now)
	if err != nil {
		t.Fatal(err)
	}

	// Second bind with same JTI should fail (JTI is spent)
	_, _, err = reg.BindBrowserSocket("sid-1", "jti-1", nil, now)
	if err != relay.ErrReplay {
		t.Errorf("expected ErrReplay for second bind, got %v", err)
	}

	// Close the session (should clean up JTI)
	_, _, err = reg.CloseSession("sid-1")
	if err != nil {
		t.Fatal(err)
	}

	// Create a new session with same JTI (simulating replay after session close)
	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-2",
		JTI:          "jti-1", // same JTI
		RelayAllowed: true,
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}

	// JTI should no longer be marked as spent since CloseSession cleaned it up
	_, _, err = reg.BindBrowserSocket("sid-2", "jti-1", nil, now)
	if err != nil {
		t.Errorf("expected JTI to be reusable after CloseSession, got error: %v", err)
	}
}

func TestRegistry_CloseSessionUnknownSID(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)

	_, _, err := reg.CloseSession("nonexistent")
	if err != relay.ErrUnknownSID {
		t.Errorf("expected ErrUnknownSID, got %v", err)
	}
}

func TestRegistry_AddForwardedBytesAccumulates(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-1",
		RelayAllowed: true,
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}

	// Multiple additions should accumulate
	reg.AddForwardedBytes("sid-1", 100)
	reg.AddForwardedBytes("sid-1", 200)
	reg.AddForwardedBytes("sid-1", 300)

	_, bytes, err := reg.CloseSession("sid-1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes != 600 {
		t.Errorf("expected accumulated bytes 600, got %d", bytes)
	}
}

func TestRegistry_AddForwardedBytesUnknownSID(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)

	// Should not panic or error for unknown SID
	reg.AddForwardedBytes("nonexistent", 100)
}

func TestRegistry_CleanupExpiredRemovesSessionAndSpentJTI(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-expired",
		JTI:          "jti-expired",
		RelayAllowed: true,
		ExpiresAt:    now.Add(100 * time.Millisecond),
	}, now); err != nil {
		t.Fatal(err)
	}

	_, _, err := reg.BindBrowserSocket("sid-expired", "jti-expired", nil, now)
	if err != nil {
		t.Fatal(err)
	}

	reg.CleanupExpired(now.Add(time.Second))

	_, err = reg.Get("sid-expired")
	if err != relay.ErrUnknownSID {
		t.Fatalf("expected ErrUnknownSID after cleanup, got %v", err)
	}

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-new",
		JTI:          "jti-expired",
		RelayAllowed: true,
		ExpiresAt:    now.Add(2 * time.Minute),
	}, now); err != nil {
		t.Fatal(err)
	}

	_, _, err = reg.BindBrowserSocket("sid-new", "jti-expired", nil, now)
	if err != nil {
		t.Fatalf("expected cleaned JTI to be reusable, got %v", err)
	}
}

func TestRegistry_CleanupExpiredKeepsActiveRelaySession(t *testing.T) {
	reg := relay.NewRegistry(2 * time.Second)
	now := time.Unix(1_800_000_000, 0)

	if err := reg.CreatePendingSession(relay.PendingSession{
		SID:          "sid-active",
		JTI:          "jti-active",
		AgentID:      "agent-1",
		RelayAllowed: true,
		ExpiresAt:    now.Add(100 * time.Millisecond),
	}, now); err != nil {
		t.Fatal(err)
	}

	if _, state, err := reg.BindAgentSocket("sid-active", "agent-1", nil, now); err != nil || state != relay.StatePendingAgent {
		t.Fatalf("agent bind = state %q, err %v; want pending agent", state, err)
	}
	if _, state, err := reg.BindBrowserSocket("sid-active", "jti-active", nil, now); err != nil || state != relay.StateActive {
		t.Fatalf("browser bind = state %q, err %v; want active", state, err)
	}

	reg.CleanupExpired(now.Add(time.Second))

	if _, err := reg.Get("sid-active"); err != nil {
		t.Fatalf("active relay session was removed at JWT expiry: %v", err)
	}
}
