package config

import (
	"os"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	// Unset any env vars that might interfere
	os.Unsetenv("DEFAULT_QUOTA_GB")

	cfg := Load()

	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}
}

func TestLoad_QuotaOverride(t *testing.T) {
	os.Setenv("DEFAULT_QUOTA_GB", "100")
	defer func() {
		os.Unsetenv("DEFAULT_QUOTA_GB")
	}()

	cfg := Load()

	if cfg.DefaultQuotaGB != 100.0 {
		t.Errorf("DefaultQuotaGB = %v, want 100.0", cfg.DefaultQuotaGB)
	}
}

// TestRelaySelectionEnabledDefaultsFalse pins the Task 20 operator flag
// contract (plan Task 20; spec §20 rollout): RELAY_SELECTION_ENABLED defaults
// to FALSE when unset — the safe rollback posture — parses Go bool literals,
// and fails closed (false) on any unparseable value.
func TestRelaySelectionEnabledDefaultsFalse(t *testing.T) {
	t.Helper()

	cases := []struct {
		env  string
		want bool
	}{
		{env: "", want: false}, // unset
		{env: "false", want: false},
		{env: "true", want: true},
		{env: "1", want: true},
		{env: "0", want: false},
		{env: "TRUE", want: true},
		{env: "bogus", want: false}, // unparseable fails closed
	}
	for _, tc := range cases {
		if tc.env == "" {
			os.Unsetenv("RELAY_SELECTION_ENABLED")
		} else {
			os.Setenv("RELAY_SELECTION_ENABLED", tc.env)
		}
		cfg := Load()
		if cfg.RelaySelectionEnabled != tc.want {
			t.Errorf("RELAY_SELECTION_ENABLED=%q: RelaySelectionEnabled = %v, want %v", tc.env, cfg.RelaySelectionEnabled, tc.want)
		}
	}
	os.Unsetenv("RELAY_SELECTION_ENABLED")
}

func TestLoad_NoLegacyTransportConfig(t *testing.T) {
	// The v1 relay/TURN/ICE transport config fields (STUNURL, RelayJWTSecret,
	// RelayPendingWaitWindow) were removed along with the transport itself.
	// This test compiles only if those fields no longer exist on Config.
	cfg := Load()

	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}
}
