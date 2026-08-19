package relay_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"sharebridge/control/internal/relay"
)

func TestSignAndVerifyBrowserPolicyJWT(t *testing.T) {
	claims := relay.BrowserPolicyClaims{
		SID:               "sid-123",
		SessionCode:       "SHARE123",
		RelayAllowed:      true,
		RelayOnly:         false,
		ExpectedStaticPub: "04abcd",
		RegisteredClaims:  jwt.RegisteredClaims{ID: "jti-123"},
	}

	token, err := relay.SignBrowserPolicyJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("SignBrowserPolicyJWT failed: %v", err)
	}

	parsed, err := relay.VerifyBrowserPolicyJWT("test-secret", token, time.Unix(1_800_000_010, 0))
	if err != nil {
		t.Fatalf("VerifyBrowserPolicyJWT failed: %v", err)
	}
	if parsed.SID != claims.SID {
		t.Errorf("SID mismatch: got %q, want %q", parsed.SID, claims.SID)
	}
	if parsed.Purpose != "browser_policy" {
		t.Errorf("Purpose mismatch: got %q, want %q", parsed.Purpose, "browser_policy")
	}
}

func TestVerifyBrowserPolicyJWT_RejectsExpiredToken(t *testing.T) {
	claims := relay.BrowserPolicyClaims{
		SID:              "sid-123",
		RegisteredClaims: jwt.RegisteredClaims{ID: "jti-123"},
	}
	token, err := relay.SignBrowserPolicyJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("SignBrowserPolicyJWT failed: %v", err)
	}

	_, err = relay.VerifyBrowserPolicyJWT("test-secret", token, time.Unix(1_800_000_200, 0))
	if err == nil {
		t.Error("expected error for expired token")
	}
}

func TestVerifyBrowserPolicyJWT_RejectsWrongPurpose(t *testing.T) {
	// Create an agent_relay token and try to verify it as browser_policy
	claims := relay.AgentRelayClaims{SID: "sid-123", AgentID: "agent-1"}
	token, err := relay.SignAgentRelayJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("SignAgentRelayJWT failed: %v", err)
	}

	_, err = relay.VerifyBrowserPolicyJWT("test-secret", token, time.Unix(1_800_000_010, 0))
	if err == nil {
		t.Error("expected error for wrong purpose token")
	}
}

func TestSignAndVerifyAgentRelayJWT(t *testing.T) {
	claims := relay.AgentRelayClaims{SID: "sid-123", AgentID: "agent-1"}
	token, err := relay.SignAgentRelayJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("SignAgentRelayJWT failed: %v", err)
	}

	parsed, err := relay.VerifyAgentRelayJWT("test-secret", token, time.Unix(1_800_000_010, 0))
	if err != nil {
		t.Fatalf("VerifyAgentRelayJWT failed: %v", err)
	}
	if parsed.Purpose != "agent_relay" {
		t.Errorf("Purpose mismatch: got %q, want %q", parsed.Purpose, "agent_relay")
	}
	if parsed.AgentID != "agent-1" {
		t.Errorf("AgentID mismatch: got %q, want %q", parsed.AgentID, "agent-1")
	}
}

func TestJTIRoundTrip_BrowserPolicy(t *testing.T) {
	jti := "jti_browser_abc123"
	claims := relay.BrowserPolicyClaims{
		SID:              "sid-123",
		RegisteredClaims: jwt.RegisteredClaims{ID: jti},
	}

	token, err := relay.SignBrowserPolicyJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("SignBrowserPolicyJWT failed: %v", err)
	}

	parsed, err := relay.VerifyBrowserPolicyJWT("test-secret", token, time.Unix(1_800_000_010, 0))
	if err != nil {
		t.Fatalf("VerifyBrowserPolicyJWT failed: %v", err)
	}

	if parsed.ID != jti {
		t.Errorf("JTI mismatch: got %q, want %q", parsed.ID, jti)
	}
}

func TestJTIRoundTrip_AgentRelay(t *testing.T) {
	jti := "jti_agent_xyz789"
	claims := relay.AgentRelayClaims{
		SID:              "sid-456",
		AgentID:          "agent-2",
		RegisteredClaims: jwt.RegisteredClaims{ID: jti},
	}

	token, err := relay.SignAgentRelayJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("SignAgentRelayJWT failed: %v", err)
	}

	parsed, err := relay.VerifyAgentRelayJWT("test-secret", token, time.Unix(1_800_000_010, 0))
	if err != nil {
		t.Fatalf("VerifyAgentRelayJWT failed: %v", err)
	}

	if parsed.ID != jti {
		t.Errorf("JTI mismatch: got %q, want %q", parsed.ID, jti)
	}
}

func TestVerifyAgentRelayJWT_RejectsWrongPurpose(t *testing.T) {
	// Create a browser_policy token and try to verify it as agent_relay
	claims := relay.BrowserPolicyClaims{SID: "sid-123", SessionCode: "SHARE123"}
	token, err := relay.SignBrowserPolicyJWT("test-secret", claims, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("SignBrowserPolicyJWT failed: %v", err)
	}

	_, err = relay.VerifyAgentRelayJWT("test-secret", token, time.Unix(1_800_000_010, 0))
	if err == nil {
		t.Error("expected error for wrong purpose token")
	}
}