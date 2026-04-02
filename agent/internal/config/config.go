package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Config holds all agent configuration settings.
// JSON tags use snake_case for file persistence.
type Config struct {
	SignalingURL      string `json:"signaling_url"`
	APIKey            string `json:"api_key,omitempty"`
	AllowedHost       string `json:"allowed_host,omitempty"`
	DefaultExpiry      int    `json:"default_expiry"`           // hours, default: 24
	DefaultMaxDownloads int   `json:"default_max_downloads"`    // 0 = unlimited, default: 10
	DefaultRelayOnly   bool   `json:"default_relay_only"`
	UIPort            int    `json:"ui_port"`                  // default: 7878
	UIPassword        string `json:"ui_password,omitempty"`

	// Legacy fields for backward compatibility
	SignalingServer string `json:"-"` // Deprecated: use SignalingURL
	Password        string `json:"-"` // Deprecated: use UIPassword
	MaxDownloads    int    `json:"-"` // Deprecated: use DefaultMaxDownloads
}

// Manager handles configuration persistence.
type Manager struct {
	filePath string
	config   *Config
}

// NewManager creates a new config manager, loading from the config file.
// Creates the config directory if it doesn't exist.
func NewManager() (*Manager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	configDir := filepath.Join(homeDir, ".opencloudshare")
	filePath := filepath.Join(configDir, "config.json")

	m := &Manager{
		filePath: filePath,
	}

	cfg, err := m.load()
	if err != nil {
		return nil, err
	}

	m.config = cfg
	return m, nil
}

// Get returns the current configuration.
func (m *Manager) Get() *Config {
	return m.config
}

// Save persists the configuration to the config file.
func (m *Manager) Save(cfg *Config) error {
	m.config = cfg
	return m.save()
}

// load reads config from file and fills empty/zero fields from env vars.
// File takes precedence over environment variables.
func (m *Manager) load() (*Config, error) {
	// Set defaults for fields where 0 is a valid value (so file can override with 0)
	cfg := &Config{
		DefaultExpiry:       24,
		DefaultMaxDownloads: 10,
		// UIPort: leave as 0, use env fallback with default
	}

	// Load from file (file overrides defaults, including setting 0 for unlimited)
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// File doesn't exist, use defaults
	} else {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("malformed config.json: %w", err)
		}
	}

	// Fill empty/zero fields from env vars (file takes precedence)
	if cfg.SignalingURL == "" {
		cfg.SignalingURL = getEnv("SIGNALING_SERVER", "ws://localhost:8080")
	}
	if cfg.APIKey == "" {
		cfg.APIKey = getEnv("OPENCLOUDSHARE_API_KEY", "")
	}
	if cfg.AllowedHost == "" {
		cfg.AllowedHost = getEnv("ALLOWED_OPENCLOUD_HOST", "")
	}
	if cfg.UIPort == 0 {
		cfg.UIPort = getEnvInt("UI_PORT", 7878)
	}
	if cfg.UIPassword == "" {
		cfg.UIPassword = getEnv("UI_PASSWORD", "")
	}

	return cfg, nil
}

// save atomically writes the configuration to the config file.
func (m *Manager) save() error {
	// Ensure directory exists
	dir := filepath.Dir(m.filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	// Marshal with indentation for readability
	data, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return err
	}

	// Write to temp file first, then rename for atomicity
	tempPath := m.filePath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0600); err != nil {
		return err
	}

	// Atomic rename
	return os.Rename(tempPath, m.filePath)
}

// Load loads configuration for backward compatibility.
// Deprecated: Use NewManager instead.
func Load() *Config {
	m, err := NewManager()
	if err != nil {
		// Fallback to env-only config on error
		return &Config{
			SignalingURL: getEnv("SIGNALING_SERVER", "ws://localhost:8080"),
			APIKey:       getEnv("OPENCLOUDSHARE_API_KEY", ""),
			AllowedHost:  getEnv("ALLOWED_OPENCLOUD_HOST", ""),
			UIPort:       getEnvInt("UI_PORT", 7878),
			UIPassword:   getEnv("UI_PASSWORD", ""),
		}
	}
	return m.Get()
}

// getEnv retrieves an environment variable or returns the default value.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getEnvInt retrieves an integer environment variable or returns the default value.
func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}