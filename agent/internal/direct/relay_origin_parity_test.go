package direct

import "testing"

// Parity vectors for the §6 relay-origin derivation. Control's
// relayctl.RelayOriginFromDirect is authoritative; the Binder's
// RelayOriginFor must agree for every production-shaped share label.
// Enrollment issues dot-free labels, so both derivations agree there. A
// dotted label is out of grammar and must never reach this function.
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

const (
	relayOriginParityNamespace = "sbd0903ed4"
	relayOriginParityBase      = "sharebridgeusercontent.com"
)

func TestRelayOriginForMatchesControlRule(t *testing.T) {
	b := NewBinder(relayOriginParityNamespace, relayOriginParityBase)
	for _, label := range relayOriginParityVectors {
		direct := label + "." + relayOriginParityNamespace + "." + relayOriginParityBase
		got, err := b.RelayOriginFor(direct)
		if err != nil {
			t.Fatalf("RelayOriginFor(%q): %v", direct, err)
		}
		want := label + ".relay." + relayOriginParityNamespace + "." + relayOriginParityBase
		if got != want {
			t.Errorf("RelayOriginFor(%q) = %q, want %q (diverged from control's RelayOriginFromDirect)", direct, got, want)
		}
	}
}

// Guard the grammar assumption the parity list rests on: this derivation must
// never be handed a dotted label (the daemon holds enrollment-issued dot-free
// share labels), and control's authoritative first-dot rule would place
// ".relay." differently than the suffix swap does — divergence for dotted
// input is why the grammar is pinned here.
func TestRelayOriginForDottedLabelDivergesFromControlRule(t *testing.T) {
	b := NewBinder(relayOriginParityNamespace, relayOriginParityBase)
	dotted := "a.b." + relayOriginParityNamespace + "." + relayOriginParityBase
	got, err := b.RelayOriginFor(dotted)
	if err != nil {
		t.Fatalf("RelayOriginFor(%q): %v", dotted, err)
	}
	controlFirstDot := "a.relay.b." + relayOriginParityNamespace + "." + relayOriginParityBase
	if got == controlFirstDot {
		t.Fatalf("RelayOriginFor unexpectedly matches control's dotted-label output %q; the dotted-grammar divergence this file documents no longer holds — re-derive the parity contract", got)
	}
}
