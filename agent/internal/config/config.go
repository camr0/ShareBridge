package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

// Config holds all agent configuration settings.
// JSON tags use snake_case for file persistence.
type Config struct {
	SignalingURL        string `json:"signaling_url"`
	APIKey              string `json:"api_key,omitempty"`
	AllowedHost         string `json:"allowed_host,omitempty"`    // OpenCloud hostname for CORS
	NCAllowedHost       string `json:"nc_allowed_host,omitempty"` // Nextcloud hostname for CORS
	AgentAPIKey         string `json:"agent_api_key,omitempty"`   // auth key for /api/v1/ JSON endpoints
	DefaultExpiry       int    `json:"default_expiry"`            // hours, default: 24
	DefaultMaxDownloads int    `json:"default_max_downloads"`     // 0 = unlimited, default: 10
	DefaultRelayOnly    bool   `json:"default_relay_only"`
	UIPort              int    `json:"ui_port"`           // default: 7878
	UIAddr              string `json:"ui_addr,omitempty"` // default: 0.0.0.0
	UIPassword          string `json:"ui_password,omitempty"`

	// Legacy fields for backward compatibility
	SignalingServer string `json:"-"` // Deprecated: use SignalingURL
	Password        string `json:"-"` // Deprecated: use UIPassword
	MaxDownloads    int    `json:"-"` // Deprecated: use DefaultMaxDownloads
}

// Manager handles configuration persistence.
type Manager struct {
	filePath           string
	config             *Config
	agentAPIKeyFromEnv bool // if true, don't persist AgentAPIKey to disk
}

// NewManager creates a new config manager, loading from the config file.
// Creates the config directory if it doesn't exist.
func NewManager() (*Manager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	configDir := filepath.Join(homeDir, ".sharebridge")
	filePath := filepath.Join(configDir, "config.json")

	m := &Manager{
		filePath: filePath,
	}

	cfg, err := m.load()
	if err != nil {
		return nil, err
	}

	m.config = cfg

	// Auto-generate AgentAPIKey if not set (first run or env override not provided)
	if m.config.AgentAPIKey == "" {
		key, err := generateAgentAPIKey()
		if err != nil {
			return nil, fmt.Errorf("generate agent API key: %w", err)
		}
		m.config.AgentAPIKey = key
		if err := m.save(); err != nil {
			log.Printf("warning: could not persist auto-generated agent API key: %v", err)
		}
	}

	return m, nil
}

// Get returns the current configuration.
func (m *Manager) Get() *Config {
	return m.config
}

// FilePath returns the path to the config file.
func (m *Manager) FilePath() string {
	return m.filePath
}

// Save persists the configuration to the config file.
func (m *Manager) Save(cfg *Config) error {
	m.config = cfg
	return m.save()
}

// load reads config from file and then applies any explicit environment
// variable overrides on top. This makes one-off local testing and temporary
// key rotation easy without editing config.json.
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

	// Apply explicit env var overrides. If an env var isn't set, keep the file
	// value and then fall back to defaults where needed.
	if v := os.Getenv("SIGNALING_SERVER"); v != "" {
		cfg.SignalingURL = v
	}
	if v := os.Getenv("SHAREBRIDGE_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("ALLOWED_SHAREBRIDGE_HOST"); v != "" {
		cfg.AllowedHost = v
	}
	if v := os.Getenv("NC_ALLOWED_SHAREBRIDGE_HOST"); v != "" {
		cfg.NCAllowedHost = v
	}
	if v := os.Getenv("SHAREBRIDGE_AGENT_API_KEY"); v != "" {
		cfg.AgentAPIKey = v
		m.agentAPIKeyFromEnv = true
	}
	if v := os.Getenv("UI_PORT"); v != "" {
		cfg.UIPort = getEnvInt("UI_PORT", cfg.UIPort)
	}
	if v := os.Getenv("UI_PASSWORD"); v != "" {
		cfg.UIPassword = v
	}
	if v := os.Getenv("UI_ADDR"); v != "" {
		cfg.UIAddr = v
	}

	if cfg.SignalingURL == "" {
		cfg.SignalingURL = "wss://sharebridge.app"
	}
	if cfg.UIPort == 0 {
		cfg.UIPort = 7878
	}
	if cfg.UIAddr == "" {
		cfg.UIAddr = "0.0.0.0"
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

	// Copy config to avoid modifying the in-memory value
	cfg := *m.config

	// Don't persist AgentAPIKey if it came from env var (allows temporary overrides)
	if m.agentAPIKeyFromEnv {
		cfg.AgentAPIKey = ""
	}

	// Marshal with indentation for readability
	data, err := json.MarshalIndent(&cfg, "", "  ")
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
			SignalingURL: getEnv("SIGNALING_SERVER", "wss://sharebridge.app"),
			APIKey:       getEnv("SHAREBRIDGE_API_KEY", ""),
			AllowedHost:  getEnv("ALLOWED_SHAREBRIDGE_HOST", ""),
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

// generateAgentAPIKey produces a "sb_agent_" prefixed 32-hex-character key
// using 16 cryptographically random bytes.
func generateAgentAPIKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand read: %w", err)
	}
	return "sb_agent_" + hex.EncodeToString(b), nil
}
