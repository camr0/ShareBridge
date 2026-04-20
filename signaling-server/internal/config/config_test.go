package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Unset any env vars that might interfere
	os.Unsetenv("DEFAULT_QUOTA_GB")
	os.Unsetenv("PROMETHEUS_URL")
	os.Unsetenv("QUOTA_CHECK_INTERVAL")
	os.Unsetenv("RELAY_PENDING_WAIT_WINDOW")

	cfg := Load()

	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}
	if cfg.PrometheusURL != "http://prometheus:9090" {
		t.Errorf("PrometheusURL = %q, want %q", cfg.PrometheusURL, "http://prometheus:9090")
	}
	if cfg.QuotaCheckInterval != 5*time.Minute {
		t.Errorf("QuotaCheckInterval = %v, want 5m", cfg.QuotaCheckInterval)
	}
	if cfg.RelayPendingWaitWindow != 7*time.Second {
		t.Errorf("RelayPendingWaitWindow = %v, want 7s", cfg.RelayPendingWaitWindow)
	}
}

func TestLoad_QuotaOverride(t *testing.T) {
	os.Setenv("DEFAULT_QUOTA_GB", "100")
	os.Setenv("PROMETHEUS_URL", "http://prom.internal:9090")
	os.Setenv("QUOTA_CHECK_INTERVAL", "2m")
	defer func() {
		os.Unsetenv("DEFAULT_QUOTA_GB")
		os.Unsetenv("PROMETHEUS_URL")
		os.Unsetenv("QUOTA_CHECK_INTERVAL")
	}()

	cfg := Load()

	if cfg.DefaultQuotaGB != 100.0 {
		t.Errorf("DefaultQuotaGB = %v, want 100.0", cfg.DefaultQuotaGB)
	}
	if cfg.PrometheusURL != "http://prom.internal:9090" {
		t.Errorf("PrometheusURL = %q", cfg.PrometheusURL)
	}
	if cfg.QuotaCheckInterval != 2*time.Minute {
		t.Errorf("QuotaCheckInterval = %v, want 2m", cfg.QuotaCheckInterval)
	}
}
