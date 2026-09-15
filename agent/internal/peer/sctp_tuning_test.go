package peer

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

// TestSCTPTuningNoOpByDefault must run before any test that sets the SB_SCTP_*
// variables: applySCTPTuning reads the process environment and apiForPeers fires
// its sync.Once, so both are process-global. Go runs tests in source order within
// a file, so keeping this one first makes the assertion deterministic.
func TestSCTPTuningNoOpByDefault(t *testing.T) {
	se := webrtc.SettingEngine{}
	if got := applySCTPTuning(&se); len(got) != 0 {
		t.Fatalf("expected no tuning with no env set, got %v", got)
	}
	if apiForPeers() != nil {
		t.Fatal("apiForPeers must return nil when nothing is configured, so pion's " +
			"defaults are used unchanged")
	}
}

func TestApplySCTPTuningAppliesEnv(t *testing.T) {
	t.Setenv("SB_SCTP_CA_STEP", "32768")
	t.Setenv("SB_SCTP_MIN_CWND", "262144")
	t.Setenv("SB_SCTP_RTO_MAX_MS", "500")
	t.Setenv("SB_SCTP_FAST_RTX_WND", "65536")

	se := webrtc.SettingEngine{}
	got := applySCTPTuning(&se)
	if len(got) != 4 {
		t.Fatalf("expected 4 settings applied, got %d: %v", len(got), got)
	}
	want := map[string]bool{
		"cwndCAStep=32768": true,
		"minCwnd=262144":   true,
		"rtoMax=500ms":     true,
		"fastRtxWnd=65536": true,
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

func TestApplySCTPTuningIgnoresGarbage(t *testing.T) {
	t.Setenv("SB_SCTP_CA_STEP", "not-a-number")
	t.Setenv("SB_SCTP_RTO_MAX_MS", "-5")
	t.Setenv("SB_SCTP_MIN_CWND", "")

	se := webrtc.SettingEngine{}
	if got := applySCTPTuning(&se); len(got) != 0 {
		t.Fatalf("expected malformed/empty values to be ignored, got %v", got)
	}
}
