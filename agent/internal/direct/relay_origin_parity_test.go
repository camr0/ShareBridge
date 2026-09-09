package direct

import "testing"

// Parity vectors for the §6 relay-origin derivation. Control's
// relayctl.RelayOriginFromDirect is authoritative; RelayOriginFor must agree
// for every production-shaped label. Control labels are dot-free 12-hex
// strings issued at enrollment, so both derivations agree there. A dotted
// label is out of grammar and must never reach this function.
//
// Keep this list IDENTICAL to the vectors in
// control/internal/relayctl/relay_origin_parity_test.go — the two derivations
// live in separate Go modules and cannot share code, so duplicated golden
// vectors are the cross-module check. If either derivation or the label
// grammar changes, update BOTH files in the same commit.
var relayOriginParityVectors = []string{
	"sbd0903ed4",
	"0123456789ab",
	"ffffffffffff",
	"sb3436bdf3aa",
}

const relayOriginParityNamespaceDomain = "sbd0903ed4.sharebridgeusercontent.com"

func TestRelayOriginForMatchesControlRule(t *testing.T) {
	for _, label := range relayOriginParityVectors {
		direct := label + "." + relayOriginParityNamespaceDomain
		got := RelayOriginFor(direct)
		want := label + ".relay." + relayOriginParityNamespaceDomain
		if got != want {
			t.Errorf("RelayOriginFor(%q) = %q, want %q (diverged from control's RelayOriginFromDirect)", direct, got, want)
		}
	}
}

// Guard the grammar assumption the parity list rests on: this derivation must
// never be handed a dotted label (the daemon holds enrollment-issued dot-free
// labels), and control's authoritative rule would place ".relay." after the
// FIRST dot — divergence for dotted input is why the grammar is pinned here.
func TestRelayOriginForDottedLabelDivergesFromControlRule(t *testing.T) {
	dotted := "a.b." + relayOriginParityNamespaceDomain
	got := RelayOriginFor(dotted)
	if got == "a.relay."+relayOriginParityNamespaceDomain {
		t.Fatalf("RelayOriginFor unexpectedly matches control's dotted-label output %q; the dotted-grammar divergence this file documents no longer holds — re-derive the parity contract", got)
	}
}
