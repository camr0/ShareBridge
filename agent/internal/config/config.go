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

const currentConfigVersion = 2

// Config holds all agent configuration settings.
// JSON tags use snake_case for file persistence.
type Config struct {
	ConfigVersion       int    `json:"config_version"`
	SignalingURL        string `json:"signaling_url"`
	APIKey              string `json:"api_key,omitempty"`
	AllowedHost         string `json:"allowed_host,omitempty"`    // OpenCloud hostname for CORS
	NCAllowedHost       string `json:"nc_allowed_host,omitempty"` // Nextcloud hostname for CORS
	AgentAPIKey         string `json:"agent_api_key,omitempty"`   // auth key for /api/v1/ JSON endpoints
	ImmichURL           string `json:"immich_url,omitempty"`
	ImmichAllowedHost   string `json:"immich_allowed_host,omitempty"`
	ImmichAPIKey        string `json:"immich_api_key,omitempty"`
	ImmichPollInterval  int    `json:"immich_poll_interval,omitempty"`
	DefaultExpiry       int    `json:"default_expiry"`        // hours, default: 24
	DefaultMaxDownloads int    `json:"default_max_downloads"` // 0 = unlimited, default: 10
	DefaultRelayOnly    bool   `json:"default_relay_only"`
	UIPort              int    `json:"ui_port"`           // default: 7878
	UIAddr              string `json:"ui_addr,omitempty"` // default: 0.0.0.0
	UIPassword          string `json:"ui_password,omitempty"`
	BaseDomain          string `json:"base_domain,omitempty"` // content base domain for direct/relay origins

	// Relay tunnel supervision settings (phase 4a §7.4). The generated frpc
	// configuration is written into the tunnel data directory; the pinned
	// frpc binary is bundled with the agent distribution and verified by CI
	// against relay/frp/manifest.json. The local target stays fixed at
	// 127.0.0.1:8443 and the relay transport CA bundle is provisioned by the
	// operator (transport verification fails closed without it).
	TunnelDataDir     string `json:"tunnel_data_dir,omitempty"`
	TunnelFRPCPath    string `json:"tunnel_frpc_path,omitempty"`
	TunnelLocalTarget string `json:"tunnel_local_target,omitempty"`
	TunnelCAFile      string `json:"tunnel_ca_file,omitempty"`

	// Legacy fields for backward compatibility
	SignalingServer string `json:"-"` // Deprecated: use SignalingURL
	Password        string `json:"-"` // Deprecated: use UIPassword
	MaxDownloads    int    `json:"-"` // Deprecated: use DefaultMaxDownloads
}

// Manager handles configuration persistence.
type Manager struct {
	filePath            string
	config              *Config
	configNeedsSave     bool
	agentAPIKeyFromEnv  bool // if true, don't persist AgentAPIKey to disk
	immichAPIKeyFromEnv bool // if true, don't persist ImmichAPIKey to disk
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
	needsSave := m.configNeedsSave

	// Auto-generate AgentAPIKey if not set (first run or env override not provided)
	if m.config.AgentAPIKey == "" {
		key, err := generateAgentAPIKey()
		if err != nil {
			return nil, fmt.Errorf("generate agent API key: %w", err)
		}
		m.config.AgentAPIKey = key
		needsSave = true
	}
	if needsSave {
		if err := m.save(); err != nil {
			log.Printf("warning: could not persist updated agent config: %v", err)
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
	cfg.ConfigVersion = currentConfigVersion
	m.config = cfg
	return m.save()
}

// load reads config from file and then applies any explicit environment
// variable overrides on top. This makes one-off local testing and temporary
// key rotation easy without editing config.json.
func (m *Manager) load() (*Config, error) {
	// Set defaults for fields where 0 is a valid value (so file can override with 0)
	cfg := &Config{
		ConfigVersion:       currentConfigVersion,
		DefaultExpiry:       24,
		DefaultMaxDownloads: 10,
		DefaultRelayOnly:    false,
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

		var metadata struct {
			ConfigVersion int `json:"config_version"`
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, fmt.Errorf("read config version: %w", err)
		}
		if metadata.ConfigVersion < currentConfigVersion {
			cfg.ConfigVersion = currentConfigVersion
			cfg.DefaultRelayOnly = false
			m.configNeedsSave = true
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
	if v := os.Getenv("IMMICH_URL"); v != "" {
		cfg.ImmichURL = v
	}
	if v := os.Getenv("IMMICH_ALLOWED_HOST"); v != "" {
		cfg.ImmichAllowedHost = v
	}
	if v := os.Getenv("IMMICH_API_KEY"); v != "" {
		cfg.ImmichAPIKey = v
		m.immichAPIKeyFromEnv = true
	}
	if v := os.Getenv("IMMICH_POLL_INTERVAL"); v != "" {
		cfg.ImmichPollInterval = getEnvInt("IMMICH_POLL_INTERVAL", cfg.ImmichPollInterval)
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
	if v := os.Getenv("CONTENT_BASE_DOMAIN"); v != "" {
		cfg.BaseDomain = v
	}
	if v := os.Getenv("SHAREBRIDGE_TUNNEL_DATA_DIR"); v != "" {
		cfg.TunnelDataDir = v
	}
	if v := os.Getenv("SHAREBRIDGE_FRPC_PATH"); v != "" {
		cfg.TunnelFRPCPath = v
	}
	if v := os.Getenv("SHAREBRIDGE_TUNNEL_LOCAL_TARGET"); v != "" {
		cfg.TunnelLocalTarget = v
	}
	if v := os.Getenv("SHAREBRIDGE_RELAY_CA_FILE"); v != "" {
		cfg.TunnelCAFile = v
	}

	if cfg.SignalingURL == "" {
		cfg.SignalingURL = "wss://sharebridge.app"
	}
	if cfg.BaseDomain == "" {
		cfg.BaseDomain = "sharebridgeusercontent.com"
	}
	if cfg.UIPort == 0 {
		cfg.UIPort = 7878
	}
	if cfg.ImmichPollInterval == 0 {
		cfg.ImmichPollInterval = 30
	}
	if cfg.UIAddr == "" {
		cfg.UIAddr = "0.0.0.0"
	}
	if cfg.TunnelDataDir == "" {
		cfg.TunnelDataDir = filepath.Join(filepath.Dir(m.filePath), "tunnel")
	}
	if cfg.TunnelFRPCPath == "" {
		cfg.TunnelFRPCPath = defaultFRPCPath()
	}
	if cfg.TunnelLocalTarget == "" {
		cfg.TunnelLocalTarget = "127.0.0.1:8443"
	}
	// TunnelCAFile intentionally defaults to empty: relay transport
	// verification fails closed until the operator provisions the relay CA
	// bundle, and the direct path never depends on it.

	return cfg, nil
}

// defaultFRPCPath resolves the bundled pinned frpc binary next to the agent
// executable (the release layout places them side by side); it falls back to
// a PATH lookup when the executable path cannot be determined.
func defaultFRPCPath() string {
	executablePath, err := os.Executable()
	if err != nil {
		return "frpc"
	}
	return filepath.Join(filepath.Dir(executablePath), "frpc")
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
	if m.immichAPIKeyFromEnv {
		cfg.ImmichAPIKey = ""
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
