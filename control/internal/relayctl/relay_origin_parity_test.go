package relayctl

import "testing"

// Parity vectors for the §6 relay-origin derivation. This function is the
// AUTHORITATIVE derivation; the agent mirrors it with direct.RelayOriginFor
// (agent/internal/direct/sni.go) because the modules cannot share code.
//
// Keep this list IDENTICAL to the vectors in
// agent/internal/direct/relay_origin_parity_test.go — duplicated golden
// vectors are the cross-module check. If either derivation or the label
// grammar changes, update BOTH files in the same commit.
var relayOriginParityVectors = []string{
	"sbd0903ed4",
	"0123456789ab",
	"ffffffffffff",
	"sb3436bdf3aa",
}

const relayOriginParityNamespaceDomain = "sbd0903ed4.sharebridgeusercontent.com"

func TestRelayOriginFromDirectGoldenVectors(t *testing.T) {
	for _, label := range relayOriginParityVectors {
		direct := label + "." + relayOriginParityNamespaceDomain
		got, err := RelayOriginFromDirect(direct)
		if err != nil {
			t.Fatalf("RelayOriginFromDirect(%q): %v", direct, err)
		}
		want := label + ".relay." + relayOriginParityNamespaceDomain
		if got != want {
			t.Errorf("RelayOriginFromDirect(%q) = %q, want %q (agent's RelayOriginFor duplicates this rule — update both parity files together)", direct, got, want)
		}
	}
}

// Documented authority: control places ".relay." after the FIRST dot, so a
// dotted label produces first.relay.rest — output the agent's derivation
// would NOT reproduce. Enrollment issues dot-free labels precisely so both
// rules agree; this test pins that the authoritative behavior is as reviewed,
// so a future change here cannot silently desync the mirror.
func TestRelayOriginFromDirectDottedLabelIsAuthoritative(t *testing.T) {
	dotted := "a.b." + relayOriginParityNamespaceDomain
	got, err := RelayOriginFromDirect(dotted)
	if err != nil {
		t.Fatalf("RelayOriginFromDirect(%q): %v", dotted, err)
	}
	if want := "a.relay.b." + relayOriginParityNamespaceDomain; got != want {
		t.Errorf("RelayOriginFromDirect(%q) = %q, want %q — the documented first-dot rule changed; re-derive the agent parity contract", dotted, got, want)
	}
}
