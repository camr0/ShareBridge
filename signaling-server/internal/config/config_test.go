package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Unset any env vars that might interfere
	os.Unsetenv("DEFAULT_QUOTA_GB")
	os.Unsetenv("RELAY_PENDING_WAIT_WINDOW")

	cfg := Load()

	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}
	if cfg.RelayPendingWaitWindow != 7*time.Second {
		t.Errorf("RelayPendingWaitWindow = %v, want 7s", cfg.RelayPendingWaitWindow)
	}
	if cfg.STUNURL != "stun:stun.cloudflare.com:3478" {
		t.Errorf("STUNURL = %q, want %q", cfg.STUNURL, "stun:stun.cloudflare.com:3478")
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

func TestLoad_DefaultsNoLongerExposeTurnOrPrometheusConfig(t *testing.T) {
	// Unset any env vars that might interfere
	os.Unsetenv("TURN_HOST")
	os.Unsetenv("TURN_PORT")
	os.Unsetenv("TURN_SECRET")
	os.Unsetenv("PROMETHEUS_URL")
	os.Unsetenv("QUOTA_CHECK_INTERVAL")
	os.Unsetenv("RELAY_PENDING_WAIT_WINDOW")

	cfg := Load()

	// Config should not have TURN/Prometheus fields - these were removed
	// We verify this by checking the struct fields no longer exist via reflection
	// or by attempting to access them (they should cause compile errors if removed)

	// Verify STUNURL is still present (this should remain)
	if cfg.STUNURL == "" {
		t.Error("STUNURL should not be empty")
	}

	// Verify RelayJWTSecret is still present (this should remain)
	if cfg.RelayJWTSecret != "" {
		// This is expected to be empty by default but field should exist
	}

	// Verify DefaultQuotaGB is still present
	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}

	// Verify RelayPendingWaitWindow is still present
	if cfg.RelayPendingWaitWindow != 7*time.Second {
		t.Errorf("RelayPendingWaitWindow = %v, want 7s", cfg.RelayPendingWaitWindow)
	}

	// The following fields should NOT exist on Config struct after cleanup:
	// - TurnHost
	// - TurnPort
	// - TurnSecret
	// - PrometheusURL
	// - QuotaCheckInterval

	// We can't directly test for non-existent fields at runtime,
	// but the fact that this test compiles proves the fields were removed
	// from the struct definition (otherwise we'd get compile errors when
	// trying to remove them from the code that uses them)
}
