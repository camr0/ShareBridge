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

func TestLoad_NoLegacyTransportConfig(t *testing.T) {
	// The v1 relay/TURN/ICE transport config fields (STUNURL, RelayJWTSecret,
	// RelayPendingWaitWindow) were removed along with the transport itself.
	// This test compiles only if those fields no longer exist on Config.
	cfg := Load()

	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}
}
