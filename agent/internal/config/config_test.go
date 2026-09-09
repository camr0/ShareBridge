package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTempConfigManager(t *testing.T) *Manager {
	t.Helper()

	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatalf("MkdirAll home: %v", err)
	}
	t.Setenv("HOME", homeDir)

	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return mgr
}

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
	if cfg.DefaultRelayOnly {
		t.Error("DefaultRelayOnly = true, want false")
	}
	if cfg.UIPort != 7878 {
		t.Errorf("UIPort = %d, want 7878", cfg.UIPort)
	}

	// AgentAPIKey auto-generation saves config during NewManager(), creating the directory.
	configDir := filepath.Join(homeDir, ".sharebridge")
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		t.Error("config directory should be created by AgentAPIKey auto-generation during NewManager()")
	}
}

func TestConfigLoadsImmichEnv(t *testing.T) {
	t.Setenv("IMMICH_URL", "http://immich.lan:2283")
	t.Setenv("IMMICH_ALLOWED_HOST", "immich.lan")
	t.Setenv("IMMICH_API_KEY", "immich-api-key")
	t.Setenv("IMMICH_POLL_INTERVAL", "45")
	mgr := newTempConfigManager(t)

	cfg := mgr.Get()
	if cfg.ImmichURL != "http://immich.lan:2283" {
		t.Fatalf("ImmichURL = %q, want http://immich.lan:2283", cfg.ImmichURL)
	}
	if cfg.ImmichAllowedHost != "immich.lan" {
		t.Fatalf("ImmichAllowedHost = %q, want immich.lan", cfg.ImmichAllowedHost)
	}
	if cfg.ImmichAPIKey != "immich-api-key" {
		t.Fatalf("ImmichAPIKey = %q, want immich-api-key", cfg.ImmichAPIKey)
	}
	if cfg.ImmichPollInterval != 45 {
		t.Fatalf("ImmichPollInterval = %d, want 45", cfg.ImmichPollInterval)
	}
}

func TestNewManager_EnvImmichAPIKeyNotPersisted(t *testing.T) {
	t.Setenv("IMMICH_API_KEY", "immich-env-secret")
	mgr := newTempConfigManager(t)

	if mgr.Get().ImmichAPIKey != "immich-env-secret" {
		t.Fatalf("ImmichAPIKey = %q, want immich-env-secret", mgr.Get().ImmichAPIKey)
	}

	data, err := os.ReadFile(mgr.FilePath())
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if strings.Contains(string(data), "immich-env-secret") {
		t.Fatalf("config file contains env-provided Immich API key")
	}
	if strings.Contains(string(data), "immich_api_key") {
		t.Fatalf("config file contains immich_api_key field, should be omitted when from env")
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
		"ui_port":        7777,
		"default_expiry": 12,
		"allowed_host":   "file.example.com",
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

func TestNewManager_MigratesLegacyRelayDefaultToDirect(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(homeDir, ".sharebridge")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)

	configPath := filepath.Join(configDir, "config.json")
	legacyConfig := []byte(`{
  "agent_api_key": "sb_agent_existing",
  "default_relay_only": true
}`)
	if err := os.WriteFile(configPath, legacyConfig, 0600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if mgr.Get().DefaultRelayOnly {
		t.Fatal("DefaultRelayOnly = true after relay-default migration, want false")
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["config_version"] != float64(currentConfigVersion) {
		t.Fatalf("config_version = %v, want %d", persisted["config_version"], currentConfigVersion)
	}
	if relayOnly, ok := persisted["default_relay_only"].(bool); !ok || relayOnly {
		t.Fatalf("persisted default_relay_only = %v, want false", persisted["default_relay_only"])
	}
}

func TestNewManager_PreservesExplicitDirectAfterRelayDefaultMigration(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	configDir := filepath.Join(homeDir, ".sharebridge")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)

	configPath := filepath.Join(configDir, "config.json")
	versionedConfig := []byte(`{
  "config_version": 1,
  "agent_api_key": "sb_agent_existing",
  "default_relay_only": false
}`)
	if err := os.WriteFile(configPath, versionedConfig, 0600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if mgr.Get().DefaultRelayOnly {
		t.Fatal("DefaultRelayOnly = true for an explicit versioned Direct preference, want false")
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

func TestNewManager_GeneratesAgentAPIKey(t *testing.T) {
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

	key := m.Get().AgentAPIKey
	if !strings.HasPrefix(key, "sb_agent_") {
		t.Errorf("AgentAPIKey %q does not start with sb_agent_", key)
	}
	// "sb_agent_" (9) + 32 hex chars from 16 random bytes
	if len(key) != 41 {
		t.Errorf("AgentAPIKey %q: expected length 41, got %d", key, len(key))
	}
}

func TestNewManager_PersistsAgentAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")

	m1, err := NewManager()
	if err != nil {
		t.Fatalf("first NewManager() error = %v", err)
	}
	key1 := m1.Get().AgentAPIKey

	m2, err := NewManager()
	if err != nil {
		t.Fatalf("second NewManager() error = %v", err)
	}
	if m2.Get().AgentAPIKey != key1 {
		t.Errorf("AgentAPIKey changed across restarts: %q -> %q", key1, m2.Get().AgentAPIKey)
	}
}

func TestNewManager_EnvOverridesAgentAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")
	os.Setenv("SHAREBRIDGE_AGENT_API_KEY", "my-custom-key")
	defer os.Unsetenv("SHAREBRIDGE_AGENT_API_KEY")

	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if m.Get().AgentAPIKey != "my-custom-key" {
		t.Errorf("AgentAPIKey = %q, want my-custom-key", m.Get().AgentAPIKey)
	}
}

func TestNewManager_EnvAgentAPIKeyNotPersisted(t *testing.T) {
	tmpDir := t.TempDir()
	homeDir := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}
	os.Setenv("HOME", homeDir)
	defer os.Unsetenv("HOME")
	os.Setenv("SHAREBRIDGE_AGENT_API_KEY", "temp-key")
	defer os.Unsetenv("SHAREBRIDGE_AGENT_API_KEY")

	// Create manager with env override
	m1, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	// Save config (simulating settings page save)
	cfg := m1.Get()
	cfg.DefaultExpiry = 48 // modify something to trigger save
	if err := m1.Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Read the file directly to verify AgentAPIKey was NOT persisted
	configPath := filepath.Join(homeDir, ".sharebridge", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if strings.Contains(string(data), "temp-key") {
		t.Errorf("config file contains env-provided AgentAPIKey 'temp-key', should not be persisted")
	}
	if strings.Contains(string(data), "agent_api_key") {
		t.Errorf("config file contains agent_api_key field, should be omitted when from env")
	}
}

// TestConnectAllowedOriginDefault verifies the fail-safe production default:
// with CONNECT_ALLOWED_ORIGIN unset the config resolves to the exact production
// interstitial origin, byte-identical to the previously hardcoded value.
func TestConnectAllowedOriginDefault(t *testing.T) {
	mgr := newTempConfigManager(t)
	if got := mgr.Get().ConnectAllowedOrigin; got != "https://sharebridge.app" {
		t.Fatalf("ConnectAllowedOrigin = %q, want default https://sharebridge.app", got)
	}
}

// TestConnectAllowedOriginEnvOverride verifies the env override feeds the
// config field verbatim when well-formed.
func TestConnectAllowedOriginEnvOverride(t *testing.T) {
	t.Setenv("CONNECT_ALLOWED_ORIGIN", "http://192.0.2.10:8080")
	mgr := newTempConfigManager(t)
	if got := mgr.Get().ConnectAllowedOrigin; got != "http://192.0.2.10:8080" {
		t.Fatalf("ConnectAllowedOrigin = %q, want http://192.0.2.10:8080", got)
	}
}

// TestConnectAllowedOriginMalformedEnvFailsClosed verifies malformed env values
// never reach the config: every one fails closed to the production default.
func TestConnectAllowedOriginMalformedEnvFailsClosed(t *testing.T) {
	for _, v := range []string{
		"not-an-origin",
		"sharebridge.app",
		"ftp://sharebridge.app",
		"https://sharebridge.app/path",
		"https://sharebridge.app?probe=1",
		"https://user:pass@sharebridge.app",
		"https://",
		"null",
	} {
		t.Setenv("CONNECT_ALLOWED_ORIGIN", v)
		mgr := newTempConfigManager(t)
		if got := mgr.Get().ConnectAllowedOrigin; got != "https://sharebridge.app" {
			t.Fatalf("CONNECT_ALLOWED_ORIGIN = %q: ConnectAllowedOrigin = %q, want fail-closed default https://sharebridge.app", v, got)
		}
	}
}

// TestValidateConnectOrigin pins the accepted grammar: an absolute origin with
// an http/https scheme, a non-empty host with an optional numeric port, and no
// userinfo, path, query, or fragment.
func TestValidateConnectOrigin(t *testing.T) {
	for _, v := range []string{
		"https://sharebridge.app",
		"http://192.0.2.10:8080",
		"http://[::1]:8080",
		"https://stun.example.com:3478",
	} {
		if err := ValidateConnectOrigin(v); err != nil {
			t.Errorf("ValidateConnectOrigin(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{
		"",
		"sharebridge.app",
		"//sharebridge.app",
		"ftp://sharebridge.app",
		"https://sharebridge.app/path",
		"https://sharebridge.app?q=1",
		"https://sharebridge.app#frag",
		"https://user:pass@sharebridge.app",
		"https://sharebridge.app:",
		"https://sharebridge.app:notaport",
		"https://sharebridge.app:0",
		"null",
		"https://",
	} {
		if err := ValidateConnectOrigin(v); err == nil {
			t.Errorf("ValidateConnectOrigin(%q) = nil, want error", v)
		}
	}
}
