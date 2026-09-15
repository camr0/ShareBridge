package peer

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// stubEnv returns an environment lookup backed by the given map. Tests use this
// rather than the process environment so they are hermetic: an earlier version
// read the real environment, which meant running `go test` with SB_SCTP_CA_STEP
// set — a documented, supported setting — would fail the "no tuning by default"
// assertion, and the assertion also depended on test execution order.
func stubEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestApplySCTPTuningIsNoOpWithoutSettings(t *testing.T) {
	se := webrtc.SettingEngine{}
	if got := applySCTPTuning(&se, stubEnv(nil)); len(got) != 0 {
		t.Fatalf("expected no tuning with an empty environment, got %v", got)
	}
}

func TestApplySCTPTuningAppliesSettings(t *testing.T) {
	se := webrtc.SettingEngine{}
	got := applySCTPTuning(&se, stubEnv(map[string]string{
		"SB_SCTP_CA_STEP":      "32768",
		"SB_SCTP_MIN_CWND":     "262144",
		"SB_SCTP_RTO_MAX_MS":   "500",
		"SB_SCTP_FAST_RTX_WND": "65536",
	}))
	want := map[string]bool{
		"cwndCAStep=32768": true,
		"minCwnd=262144":   true,
		"rtoMax=500ms":     true,
		"fastRtxWnd=65536": true,
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d settings applied, got %d: %v", len(want), len(got), got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected setting %q (applied: %v)", g, got)
		}
		delete(want, g)
	}
	for missing := range want {
		t.Errorf("setting %q was not applied", missing)
	}
}

func TestApplySCTPTuningIgnoresMalformedValues(t *testing.T) {
	se := webrtc.SettingEngine{}
	got := applySCTPTuning(&se, stubEnv(map[string]string{
		"SB_SCTP_CA_STEP":    "not-a-number",
		"SB_SCTP_RTO_MAX_MS": "-5",
		"SB_SCTP_MIN_CWND":   "",
	}))
	if len(got) != 0 {
		t.Fatalf("expected malformed and empty values to be ignored, got %v", got)
	}
}

// TestAPIIsDefaultWithoutSettings exercises the real selection path with every
// SB_SCTP_* variable removed. It runs in a subprocess because apiForPeers caches
// its result in a sync.Once: whichever caller runs first in the test binary fixes
// the outcome for the rest of the run, so this cannot be asserted in-process.
func TestAPIIsDefaultWithoutSettings(t *testing.T) {
	if os.Getenv("SB_TEST_CHILD") == "1" {
		if api := apiForPeers(); api != nil {
			t.Fatal("apiForPeers returned a custom API with no SB_SCTP_* variables set")
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestAPIIsDefaultWithoutSettings")
	cmd.Env = append(withoutSCTPEnv(os.Environ()), "SB_TEST_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("subprocess check failed: %v\n%s", err, out)
	}
}

// withoutSCTPEnv returns env with every SB_SCTP_* entry removed.
func withoutSCTPEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "SB_SCTP_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
