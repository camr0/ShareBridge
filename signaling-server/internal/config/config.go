package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port    string
	STUNURL string
	DataDir string

	// TURN configuration
	TurnHost   string
	TurnPort   string
	TurnSecret string

	// Relay configuration
	RelayJWTSecret         string
	RelayPendingWaitWindow time.Duration

	// SMTP (optional - if absent, email verification is disabled and login is allowed immediately)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string

	// Bandwidth quota — always enforced when TURN is configured (requires Prometheus)
	DefaultQuotaGB     float64
	PrometheusURL      string
	QuotaCheckInterval time.Duration
}

func Load() *Config {
	return &Config{
		Port:    getEnv("PORT", "8080"),
		STUNURL: getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DataDir: getEnv("DATA_DIR", "./pb_data"),

		TurnHost:   getEnv("TURN_HOST", ""),
		TurnPort:   getEnv("TURN_PORT", "3478"),
		TurnSecret: getEnv("TURN_SECRET", ""),

		RelayJWTSecret:         getEnv("RELAY_JWT_SECRET", ""),
		RelayPendingWaitWindow: getEnvDuration("RELAY_PENDING_WAIT_WINDOW", 7*time.Second),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),

		DefaultQuotaGB:     getEnvFloat("DEFAULT_QUOTA_GB", 50.0),
		PrometheusURL:      getEnv("PROMETHEUS_URL", "http://prometheus:9090"),
		QuotaCheckInterval: getEnvDuration("QUOTA_CHECK_INTERVAL", 5*time.Minute),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return def
}

// HasTurn returns true if TURN is configured.
func (c *Config) HasTurn() bool {
	return c.TurnHost != "" && c.TurnSecret != ""
}

// TurnURL returns the TURN URL (e.g., "turn:yourdomain.com:3478").
func (c *Config) TurnURL() string {
	return fmt.Sprintf("turn:%s:%s", c.TurnHost, c.TurnPort)
}

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool {
	return c.SMTPHost != ""
}
