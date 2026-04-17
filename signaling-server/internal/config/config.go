package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port    string
	DataDir string

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
	DefaultQuotaGB     float64
	QuotaCheckInterval time.Duration
}

func Load() *Config {
	ttl, _ := time.ParseDuration(getEnv("JWT_TTL", "5m"))
	return &Config{
		Port:                getEnv("PORT", "8080"),
		DataDir:             getEnv("DATA_DIR", "./pb_data"),
		RelayListenAddr:     getEnv("RELAY_LISTEN_ADDR", "/ip4/127.0.0.1/tcp/9001/ws"),
		RelayAnnounceAddr:   getEnv("RELAY_ANNOUNCE_ADDR", ""),
		RelayPrivateKeyPath: getEnv("RELAY_PRIVATE_KEY_PATH", ""),
		JWTSecret:           []byte(getEnv("JWT_SECRET", "")),
		JWTTTL:              ttl,
		SMTPHost:            getEnv("SMTP_HOST", ""),
		SMTPPort:            getEnv("SMTP_PORT", "587"),
		SMTPUser:            getEnv("SMTP_USER", ""),
		SMTPPassword:        getEnv("SMTP_PASSWORD", ""),
		DefaultQuotaGB:      getEnvFloat("DEFAULT_QUOTA_GB", 50.0),
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