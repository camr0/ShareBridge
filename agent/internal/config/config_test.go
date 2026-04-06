package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNewManager_CreatesConfigDir(t *testing.T) {
	// Create a temp home directory
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Set HOME environment variable
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", homeDir)
	defer os.Setenv("HOME", origHome)

	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// Check that config has defaults
	cfg := m.Get()
	if cfg.DefaultExpiry != 24 {
		t.Errorf("DefaultExpiry = %d, want 24", cfg.DefaultExpiry)
	}
	if cfg.DefaultMaxDownloads != 10 {
		t.Errorf("DefaultMaxDownloads = %d, want 10", cfg.DefaultMaxDownloads)
	}
	if cfg.UIPort != 7878 {
		t.Errorf("UIPort = %d, want 7878", cfg.UIPort)
	}

	// Directory is created on save, not on load
	// Verify it doesn't exist yet
	configDir := filepath.Join(homeDir, ".sharebridge")
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Error("config directory should not exist before save")
	}

	// After save, directory should exist
	if err := m.Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		t.Error("config directory was not created after save")
	}
}

func TestLoad_EnvVarFallback(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}

	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")

	// Set env vars
	os.Setenv("SIGNALING_SERVER", "ws://test.example.com:8080")
	os.Setenv("SHAREBRIDGE_API_KEY", "test-api-key")
	os.Setenv("ALLOWED_SHAREBRIDGE_HOST", "cloud.example.com")
	os.Setenv("UI_PORT", "9999")
	os.Setenv("UI_PASSWORD", "secret123")
	defer func() {
		os.Unsetenv("SIGNALING_SERVER")
		os.Unsetenv("SHAREBRIDGE_API_KEY")
		os.Unsetenv("ALLOWED_SHAREBRIDGE_HOST")
		os.Unsetenv("UI_PORT")
		os.Unsetenv("UI_PASSWORD")
	}()

	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	cfg := m.Get()
	if cfg.SignalingURL != "ws://test.example.com:8080" {
		t.Errorf("SignalingURL = %q, want %q", cfg.SignalingURL, "ws://test.example.com:8080")
	}
	if cfg.APIKey != "test-api-key" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "test-api-key")
	}
	if cfg.AllowedHost != "cloud.example.com" {
		t.Errorf("AllowedHost = %q, want %q", cfg.AllowedHost, "cloud.example.com")
	}
	if cfg.UIPort != 9999 {
		t.Errorf("UIPort = %d, want 9999", cfg.UIPort)
	}
	if cfg.UIPassword != "secret123" {
		t.Errorf("UIPassword = %q, want %q", cfg.UIPassword, "secret123")
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(homeDir, ".sharebridge")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}

	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")

	// Set env vars (these should override file values)
	os.Setenv("SIGNALING_SERVER", "ws://env.example.com")
	os.Setenv("SHAREBRIDGE_API_KEY", "env-api-key")
	os.Setenv("UI_PORT", "1111")
	defer func() {
		os.Unsetenv("SIGNALING_SERVER")
		os.Unsetenv("SHAREBRIDGE_API_KEY")
		os.Unsetenv("UI_PORT")
	}()

	// Write config file
	fileConfig := Config{
		SignalingURL:        "ws://file.example.com:9090",
		APIKey:              "file-api-key",
		DefaultExpiry:       48,
		DefaultMaxDownloads: 20,
		UIPort:              8888,
	}
	data, err := json.MarshalIndent(fileConfig, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	cfg := m.Get()
	// Env values should override the file
	if cfg.SignalingURL != "ws://env.example.com" {
		t.Errorf("SignalingURL = %q, want %q", cfg.SignalingURL, "ws://env.example.com")
	}
	if cfg.APIKey != "env-api-key" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "env-api-key")
	}
	// File-only values should still be kept
	if cfg.DefaultExpiry != 48 {
		t.Errorf("DefaultExpiry = %d, want 48", cfg.DefaultExpiry)
	}
	if cfg.DefaultMaxDownloads != 20 {
		t.Errorf("DefaultMaxDownloads = %d, want 20", cfg.DefaultMaxDownloads)
	}
	if cfg.UIPort != 1111 {
		t.Errorf("UIPort = %d, want 1111", cfg.UIPort)
	}
}

func TestLoad_PartialFileWithEnvFill(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(homeDir, ".sharebridge")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}

	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")

	// Set env vars
	os.Setenv("SIGNALING_SERVER", "ws://env.example.com")
	os.Setenv("SHAREBRIDGE_API_KEY", "env-api-key")
	defer func() {
		os.Unsetenv("SIGNALING_SERVER")
		os.Unsetenv("SHAREBRIDGE_API_KEY")
	}()

	// Write partial config file (missing SignalingURL and APIKey)
	fileConfig := map[string]interface{}{
		"ui_port":            7777,
		"default_expiry":     12,
		"allowed_host":       "file.example.com",
	}
	data, err := json.MarshalIndent(fileConfig, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	cfg := m.Get()
	// File values should be used when there is no env override.
	if cfg.UIPort != 7777 {
		t.Errorf("UIPort = %d, want 7777", cfg.UIPort)
	}
	if cfg.DefaultExpiry != 12 {
		t.Errorf("DefaultExpiry = %d, want 12", cfg.DefaultExpiry)
	}
	if cfg.AllowedHost != "file.example.com" {
		t.Errorf("AllowedHost = %q, want %q", cfg.AllowedHost, "file.example.com")
	}
	// Env vars should fill empty fields.
	if cfg.SignalingURL != "ws://env.example.com" {
		t.Errorf("SignalingURL = %q, want %q", cfg.SignalingURL, "ws://env.example.com")
	}
	if cfg.APIKey != "env-api-key" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "env-api-key")
	}
	// Default should still apply
	if cfg.DefaultMaxDownloads != 10 {
		t.Errorf("DefaultMaxDownloads = %d, want 10", cfg.DefaultMaxDownloads)
	}
}

func TestSave(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}

	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")

	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// Save a new config
	newConfig := &Config{
		SignalingURL:        "ws://saved.example.com:8080",
		APIKey:              "saved-api-key",
		AllowedHost:         "saved.example.com",
		DefaultExpiry:       72,
		DefaultMaxDownloads: 50,
		DefaultRelayOnly:    true,
		UIPort:              9000,
		UIPassword:          "saved-password",
	}

	if err := m.Save(newConfig); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Load again and verify
	m2, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() second call error = %v", err)
	}

	loaded := m2.Get()
	if loaded.SignalingURL != newConfig.SignalingURL {
		t.Errorf("SignalingURL = %q, want %q", loaded.SignalingURL, newConfig.SignalingURL)
	}
	if loaded.APIKey != newConfig.APIKey {
		t.Errorf("APIKey = %q, want %q", loaded.APIKey, newConfig.APIKey)
	}
	if loaded.AllowedHost != newConfig.AllowedHost {
		t.Errorf("AllowedHost = %q, want %q", loaded.AllowedHost, newConfig.AllowedHost)
	}
	if loaded.DefaultExpiry != newConfig.DefaultExpiry {
		t.Errorf("DefaultExpiry = %d, want %d", loaded.DefaultExpiry, newConfig.DefaultExpiry)
	}
	if loaded.DefaultMaxDownloads != newConfig.DefaultMaxDownloads {
		t.Errorf("DefaultMaxDownloads = %d, want %d", loaded.DefaultMaxDownloads, newConfig.DefaultMaxDownloads)
	}
	if loaded.DefaultRelayOnly != newConfig.DefaultRelayOnly {
		t.Errorf("DefaultRelayOnly = %v, want %v", loaded.DefaultRelayOnly, newConfig.DefaultRelayOnly)
	}
	if loaded.UIPort != newConfig.UIPort {
		t.Errorf("UIPort = %d, want %d", loaded.UIPort, newConfig.UIPort)
	}
	if loaded.UIPassword != newConfig.UIPassword {
		t.Errorf("UIPassword = %q, want %q", loaded.UIPassword, newConfig.UIPassword)
	}
}

func TestLoad_BackwardCompatibility(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}

	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")

	// Test deprecated Load() function
	cfg := Load()
	if cfg == nil {
		t.Fatal("Load() returned nil")
	}

	// Should have defaults
	if cfg.DefaultExpiry != 24 {
		t.Errorf("DefaultExpiry = %d, want 24", cfg.DefaultExpiry)
	}
	if cfg.DefaultMaxDownloads != 10 {
		t.Errorf("DefaultMaxDownloads = %d, want 10", cfg.DefaultMaxDownloads)
	}
	if cfg.UIPort != 7878 {
		t.Errorf("UIPort = %d, want 7878", cfg.UIPort)
	}
}

func TestGetEnvInt(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		def      int
		want     int
	}{
		{"empty uses default", "", 10, 10},
		{"valid number", "42", 10, 42},
		{"zero value", "0", 10, 0},
		{"negative", "-5", 10, -5},
		{"invalid uses default", "abc", 10, 10},
		{"invalid with spaces", "  5  ", 10, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := "TEST_INT_ENV"
			if tt.envValue != "" {
				os.Setenv(key, tt.envValue)
				defer os.Unsetenv(key)
			}

			got := getEnvInt(key, tt.def)
			if got != tt.want {
				t.Errorf("getEnvInt() = %d, want %d", got, tt.want)
			}
		})
	}
}
