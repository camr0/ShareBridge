package turn

import (
	"testing"
)

func TestBuildICEConfig_NoTurn(t *testing.T) {
	cfg := &ICEConfigRequest{
		STUNURL: "stun:stun.cloudflare.com:3478",
	}

	servers := BuildICEConfig(cfg)

	if len(servers) != 1 {
		t.Errorf("expected 1 ICE server, got %d", len(servers))
	}
	if servers[0].URLs[0] != "stun:stun.cloudflare.com:3478" {
		t.Errorf("unexpected STUN URL: %s", servers[0].URLs[0])
	}
}

func TestBuildICEConfig_WithTurn(t *testing.T) {
	creds := Credentials{
		Username:   "1700000000:abc123",
		Credential: "test-credential",
	}

	cfg := &ICEConfigRequest{
		STUNURL:     "stun:stun.cloudflare.com:3478",
		TurnURL:     "turn:localhost:3478",
		Credentials: &creds,
	}

	servers := BuildICEConfig(cfg)

	if len(servers) != 2 {
		t.Errorf("expected 2 ICE servers (STUN + TURN), got %d", len(servers))
	}

	// STUN should be first
	if servers[0].URLs[0] != "stun:stun.cloudflare.com:3478" {
		t.Errorf("expected STUN first, got: %s", servers[0].URLs[0])
	}

	// TURN should be second with credentials
	if servers[1].URLs[0] != "turn:localhost:3478" {
		t.Errorf("expected TURN second, got: %s", servers[1].URLs[0])
	}
	if servers[1].Username != "1700000000:abc123" {
		t.Errorf("TURN username mismatch: %s", servers[1].Username)
	}
}
