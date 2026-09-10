package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	UIAddr              string `json:"ui_addr,omitempty"` // default: 127.0.0.1 (loopback only)
	UIPassword          string `json:"ui_password,omitempty"`
	BaseDomain          string `json:"base_domain,omitempty"` // content base domain for direct/relay origins

	// ConnectAllowedOrigin is the single cross-origin caller of the direct
	// path's /connect reachability check (§9.3): the control-hosted
	// interstitial origin. Production keeps the default
	// (https://sharebridge.app); test deployments override it via the
	// CONNECT_ALLOWED_ORIGIN env var to the test interstitial's origin.
	// Values are validated at load (ValidateConnectOrigin); malformed values
	// fail closed to the default.
	ConnectAllowedOrigin string `json:"connect_allowed_origin,omitempty"`

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
	if v := os.Getenv("CONNECT_ALLOWED_ORIGIN"); v != "" {
		cfg.ConnectAllowedOrigin = v
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
		// Secure by default: an unset address binds loopback only. Any
		// non-loopback bind is an explicit opt-in and requires UI_PASSWORD
		// (enforced by ValidateAdminBind at startup).
		cfg.UIAddr = defaultUIAddr
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
	// ConnectAllowedOrigin: file value stands only when well-formed; an empty
	// or malformed value (from either the file or the env override above)
	// fails closed to the production default — the connect check must never
	// end up comparing against an unusable origin.
	if cfg.ConnectAllowedOrigin == "" {
		cfg.ConnectAllowedOrigin = defaultConnectAllowedOrigin
	} else if err := ValidateConnectOrigin(cfg.ConnectAllowedOrigin); err != nil {
		log.Printf("config: invalid connect allowed origin %q (%v); failing closed to default %q",
			cfg.ConnectAllowedOrigin, err, defaultConnectAllowedOrigin)
		cfg.ConnectAllowedOrigin = defaultConnectAllowedOrigin
	}
	// TunnelCAFile intentionally defaults to empty: relay transport
	// verification fails closed until the operator provisions the relay CA
	// bundle, and the direct path never depends on it.

	return cfg, nil
}

// defaultUIAddr is the secure default bind for the admin UI: loopback only,
// so a fresh deployment is never reachable off-host.
const defaultUIAddr = "127.0.0.1"

// NormalizeUIAddr returns addr with the secure default applied when it is
// empty, so an unset address never means "all interfaces" (which is what a
// bare ":port" listen address would do).
func NormalizeUIAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return defaultUIAddr
	}
	return addr
}

// IsLoopbackAddr reports whether addr refers only to the local host. It treats
// 127.0.0.0/8, ::1 and localhost (with or without a port) as loopback; an
// empty address normalizes to the loopback default. Anything else — including
// 0.0.0.0 and :: — is not loopback.
func IsLoopbackAddr(addr string) bool {
	a := NormalizeUIAddr(addr)
	if host, _, err := net.SplitHostPort(a); err == nil {
		a = host
	}
	a = strings.Trim(strings.TrimSpace(a), "[]")
	if a == "" {
		return false
	}
	if strings.EqualFold(a, "localhost") {
		return true
	}
	ip := net.ParseIP(a)
	return ip != nil && ip.IsLoopback()
}

// ValidateAdminBind enforces the fail-closed startup rule for the admin UI: a
// non-loopback bind with no configured password is refused, because it would
// expose an unauthenticated admin surface (settings, share create/revoke,
// secret rendering) to the network. Loopback binds stay passwordless (local
// trust), and any bind is allowed once a password is set. The error names both
// settings so the fix is actionable.
func ValidateAdminBind(addr, password string) error {
	if password != "" {
		return nil
	}
	if IsLoopbackAddr(addr) {
		return nil
	}
	return fmt.Errorf(
		"refusing to start: UI_ADDR=%q binds a non-loopback interface but UI_PASSWORD is empty; "+
			"set UI_PASSWORD to require authentication or set UI_ADDR=%s to serve the admin UI on loopback only",
		NormalizeUIAddr(addr), defaultUIAddr)
}

// ValidateAdminBind applies ValidateAdminBind to the configuration's admin
// bind address and password.
func (c *Config) ValidateAdminBind() error {
	if c == nil {
		return nil
	}
	return ValidateAdminBind(c.UIAddr, c.UIPassword)
}

// defaultConnectAllowedOrigin is the production interstitial origin (§9.3);
// the connect handler compares request Origins against the resolved value
// byte-exactly and never reflects a request-supplied Origin.
const defaultConnectAllowedOrigin = "https://sharebridge.app"

// ValidateConnectOrigin reports whether v is an acceptable connect allowed
// origin: an absolute origin with an http/https scheme, a non-empty host with
// an optional numeric port, and no userinfo, path, query, or fragment — the
// exact shape a browser sends in an Origin header. Anything else is rejected
// so a typo fails closed to the default instead of silently breaking (or
// broadening) the connect check.
func ValidateConnectOrigin(v string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("parse origin: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q: want http or https", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("userinfo not allowed")
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("origin must not carry a path, query, or fragment")
	}
	if err := validateOriginHostPort(u.Host); err != nil {
		return err
	}
	return nil
}

// validateOriginHostPort validates the host[:port] part of an origin: the port
// suffix, when present, must be numeric in range 1-65535; a bare bracketed
// IPv6 literal is allowed.
func validateOriginHostPort(host string) error {
	if h, port, err := net.SplitHostPort(host); err == nil {
		if h == "" {
			return fmt.Errorf("empty host")
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid port %q", port)
		}
		return nil
	}
	// SplitHostPort failed: the only acceptable form left is a bare bracketed
	// IPv6 literal (no port).
	if strings.HasPrefix(host, "[") {
		if !strings.HasSuffix(host, "]") || len(host) <= len("[]") {
			return fmt.Errorf("invalid host %q", host)
		}
		return nil
	}
	if strings.Contains(host, ":") {
		return fmt.Errorf("invalid host %q", host)
	}
	return nil
}

// ResolveConnectAllowedOrigin resolves the effective connect allowed origin
// from the environment: CONNECT_ALLOWED_ORIGIN when set and well-formed, the
// production default otherwise (fail closed). The direct server calls this at
// construction so a test deployment's override takes effect without any other
// wiring; production behavior is byte-identical when the variable is unset.
func ResolveConnectAllowedOrigin() string {
	v := os.Getenv("CONNECT_ALLOWED_ORIGIN")
	if v == "" {
		return defaultConnectAllowedOrigin
	}
	if err := ValidateConnectOrigin(v); err != nil {
		log.Printf("config: invalid CONNECT_ALLOWED_ORIGIN %q (%v); failing closed to default %q",
			v, err, defaultConnectAllowedOrigin)
		return defaultConnectAllowedOrigin
	}
	return v
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
			UIAddr:       getEnv("UI_ADDR", defaultUIAddr),
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
