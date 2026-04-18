package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port    string
	DataDir string
	DebugWS bool

	// Relay (libp2p Host)
	RelayListenAddr     string        // multiaddr, e.g. "/ip4/127.0.0.1/tcp/9001/ws"
	RelayAnnounceAddr   string        // public multiaddr, e.g. "/dns4/relay.sharebridge.app/tcp/443/wss"
	RelayPrivateKeyPath string        // PEM file; empty = ephemeral (dev only)
	JWTSecret           []byte
	JWTTTL              time.Duration

	// SMTP (optional)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string

	// Bandwidth quota
	DefaultQuotaGB        float64       // default relay quota for new accounts
	RelayMaxCircuitDataGB float64       // max bytes per relay circuit (0 = unlimited)
	QuotaCheckInterval   time.Duration
}

func Load() *Config {
	ttl, _ := time.ParseDuration(getEnv("JWT_TTL", "5m"))
	defaultQuotaGB := getEnvFloat("DEFAULT_QUOTA_GB", 50.0)
	return &Config{
		Port:                getEnv("PORT", "8080"),
		DataDir:             getEnv("DATA_DIR", "./pb_data"),
		DebugWS:             getEnvBool("DEBUG_WS", false),
		RelayListenAddr:     getEnv("RELAY_LISTEN_ADDR", "/ip4/127.0.0.1/tcp/9001/ws"),
		RelayAnnounceAddr:   getEnv("RELAY_ANNOUNCE_ADDR", ""),
		RelayPrivateKeyPath: getEnv("RELAY_PRIVATE_KEY_PATH", ""),
		JWTSecret:           []byte(getEnv("JWT_SECRET", "")),
		JWTTTL:              ttl,
		SMTPHost:            getEnv("SMTP_HOST", ""),
		SMTPPort:            getEnv("SMTP_PORT", "587"),
		SMTPUser:            getEnv("SMTP_USER", ""),
		SMTPPassword:        getEnv("SMTP_PASSWORD", ""),
		DefaultQuotaGB:      defaultQuotaGB,
		RelayMaxCircuitDataGB: getEnvFloat("RELAY_MAX_CIRCUIT_DATA_GB", defaultQuotaGB),
		QuotaCheckInterval:  getEnvDuration("QUOTA_CHECK_INTERVAL", 5*time.Minute),
	}
}

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool { return c.SMTPHost != "" }

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
